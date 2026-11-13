package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

type livenessConfig struct {
	enabled     bool
	interval    time.Duration
	platforms   []string
	concurrency int
}

func (s *Server) livenessConfig() (livenessConfig, error) {
	return s.livenessConfigWith(nil)
}

func (s *Server) livenessConfigWith(updates map[string]string) (livenessConfig, error) {
	cfg := livenessConfig{interval: 6 * time.Hour, platforms: []string{"chatgpt"}, concurrency: 1}
	entries, err := s.env.Parse()
	if err != nil {
		return cfg, err
	}
	values := map[string]string{}
	for _, item := range entries {
		if _, exists := values[item.Key]; !exists {
			values[item.Key] = item.Value
		}
	}
	for key, value := range updates {
		values[key] = value
	}
	value := func(key string) string {
		if v, exists := values[key]; exists {
			return strings.TrimSpace(v)
		}
		if v, exists := settings.DeploymentValue(key); exists {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(values[key])
	}
	switch strings.ToLower(value("REG_FACTORY_LIVENESS_ENABLED")) {
	case "1", "true", "yes", "on":
		cfg.enabled = true
	default:
		return cfg, nil
	}
	if v := value("REG_FACTORY_LIVENESS_INTERVAL"); v != "" {
		cfg.interval, err = time.ParseDuration(v)
		if err != nil || cfg.interval < time.Minute {
			return cfg, fmt.Errorf("巡检周期必须是至少 1m 的时间间隔")
		}
	}
	if v := value("REG_FACTORY_LIVENESS_PLATFORMS"); v != "" {
		cfg.platforms = nil
		for _, platform := range strings.Split(v, ",") {
			platform = strings.ToLower(strings.TrimSpace(platform))
			if !slices.Contains([]string{"chatgpt", "claude", "grok", "kiro", "outlook"}, platform) {
				return cfg, fmt.Errorf("巡检平台无效")
			}
			if !slices.Contains(cfg.platforms, platform) {
				cfg.platforms = append(cfg.platforms, platform)
			}
		}
	}
	if v := value("REG_FACTORY_LIVENESS_ACCOUNT_CONCURRENCY"); v != "" {
		cfg.concurrency, err = strconv.Atoi(v)
		if err != nil || cfg.concurrency < 1 || cfg.concurrency > 10 {
			return cfg, fmt.Errorf("其他平台巡检并发必须为 1–10")
		}
	}
	return cfg, nil
}

type livenessSchedule struct {
	key  string
	next time.Time
}

// StartLivenessLoop rereads saved configuration. Enabling/restarting waits one
// full interval; it never performs an implicit first scan or missed-run catchup.
func (s *Server) StartLivenessLoop(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		state := livenessSchedule{}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			if err := s.livenessTick(ctx, time.Now(), &state); err != nil {
				log.Printf("[liveness] 巡检未发起: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func (s *Server) livenessTick(ctx context.Context, at time.Time, state *livenessSchedule) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cfg, err := s.livenessConfig()
	if err != nil || !cfg.enabled {
		*state = livenessSchedule{}
		return err
	}
	key := fmt.Sprintf("%s/%v/%d", cfg.interval, cfg.platforms, cfg.concurrency)
	if state.key != key {
		state.key, state.next = key, at.Add(cfg.interval)
		return nil
	}
	if at.Before(state.next) {
		return nil
	}
	for _, platform := range cfg.platforms {
		if platform == "chatgpt" {
			id, err := s.st.ScheduleLivenessCheck(at, cfg.interval)
			if err != nil {
				return err
			}
			if id != "" {
				s.st.Audit("scheduler", "account.check", id, "")
			}
			continue
		}
		accounts, err := s.st.ListAccounts("", platform, "", 1)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			continue
		}
		busy, err := s.st.HasActiveLivenessScan(platform)
		if err != nil {
			return err
		}
		if busy {
			continue
		}
		args, _ := json.Marshal([]string{"scan", "--platform", platform, "--account-concurrency", strconv.Itoa(cfg.concurrency)})
		task := store.Task{ID: fmt.Sprintf("liveness-%s-%d", platform, at.UnixNano()), Script: "asset_ops",
			Title: "巡检 · " + platform, ArgsJSON: string(args), Actor: "scheduler"}
		if err = s.run.Submit(task); err != nil {
			return err
		}
	}
	state.next = at.Add(cfg.interval)
	return nil
}
