package api

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/centralasset"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
)

// Both the form and raw editor use the same validation and recovery boundary.
// The caller serializes save + archive + atomic replacement with configMu.
func (s *Server) validateConfiguration(updates map[string]string) error {
	schema, err := settings.LoadConfigSchema(filepath.Join(s.cfg.AppRoot, "deploy", "config.schema.json"))
	if err != nil {
		return fmt.Errorf("配置定义读取失败，请重试")
	}
	fields := map[string]settings.ConfigField{}
	for _, group := range schema {
		for _, field := range group.Items {
			fields[field.Key] = field
		}
	}
	for key, value := range updates {
		field := fields[key]
		if (settings.DeploymentOnly(key) || field.ReadOnly) && value != s.env.Get(key) {
			return fmt.Errorf("%s 是部署信息，请在对应部署或来源管理入口修改", key)
		}
		if value == "" {
			continue
		}
		if field.Type == "int" {
			if _, err := strconv.Atoi(value); err != nil {
				return fmt.Errorf("%s 必须填写整数", key)
			}
		}
		if field.Type == "bool" {
			switch strings.ToLower(value) {
			case "true", "false", "1", "0", "yes", "no", "on", "off":
			default:
				return fmt.Errorf("%s 的开关值无效", key)
			}
		} else if len(field.Choices) > 0 {
			found := false
			for _, choice := range field.Choices {
				if choice == value {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%s 请选择支持的模式", key)
			}
		}
		switch key {
		case "REG_FACTORY_WORK_ORIGIN":
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(value, " \t\r\n") {
				return fmt.Errorf("Work 地址需为完整的 http(s) 地址，不包含账号、路径或查询参数")
			}
		case "REG_FACTORY_ASSET_KEY", "REG_FACTORY_ASSET_JOURNAL_KEY", "REG_FACTORY_NODE_ARTIFACT_KEY":
			raw, err := base64.URLEncoding.Strict().DecodeString(value)
			if err != nil || len(raw) != 32 {
				return fmt.Errorf("%s 需要 32 字节 base64url 密钥，可点击生成", key)
			}
		case "REG_FACTORY_NODE_KEY", "REG_FACTORY_NODE_RECEIPT_KEY", "REG_FACTORY_NODE_BUSINESS_KEY":
			if len(value) < 32 {
				return fmt.Errorf("%s 至少需要 32 字节，可点击生成", key)
			}
		}
	}
	for key, limits := range map[string][2]int{"REG_FACTORY_MAX_CONCURRENCY": {1, 32}, "REG_FACTORY_MEMORY_GUARD_PCT": {1, 100}, "REGD_LOG_TAIL": {1, 10000}} {
		if value, ok := updates[key]; ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < limits[0] || n > limits[1] {
				return fmt.Errorf("%s 范围为 %d–%d", key, limits[0], limits[1])
			}
		}
	}
	if _, err := s.livenessConfigWith(updates); err != nil {
		return err
	}
	env, err := settings.WorkerEnvironment(s.cfg.EnvFile)
	if err != nil {
		return err
	}
	current, _ := centralasset.FromEnvironment(s.cfg.DataDir, env)
	changed := false
	for key, value := range updates {
		if (strings.HasPrefix(key, "REG_FACTORY_ASSET_") || key == "REG_FACTORY_WORK_ORIGIN" || strings.HasPrefix(key, "REG_FACTORY_NODE_")) && value != s.env.Effective(key) {
			changed = true
		}
	}
	if changed {
		if next, ok := updates["REG_FACTORY_NODE_ARTIFACT_KEY"]; ok && next != s.env.Effective("REG_FACTORY_NODE_ARTIFACT_KEY") {
			if err := settings.RetainArtifactKey(s.cfg.EnvFile, s.env.Effective("REG_FACTORY_NODE_ARTIFACT_KEY")); err != nil {
				return fmt.Errorf("原成品解密密钥未能保留，本次配置未修改")
			}
		}
		if err := centralasset.PreserveConfig(current); err != nil {
			return fmt.Errorf("原任务恢复配置未能保存，本次配置未修改")
		}
	}
	return nil
}
