package plus

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
)

// Stage 是流水线的一个阶段。每个阶段可以独立选出口。
//
// 为什么需要分阶段
// --------------
// 提链（link）和绑卡（bind）对出口的要求不一样：
//
//	· 提链只要求「建单国家 == 出口国家」，用哪国都行
//	· 绑卡还要「卡的发卡国 == 出口国家」，约束更严
//
// 而且两条请求链在 Stripe/OpenAI 侧是不同的信任域：
//
//	· 提链打 chatgpt.com
//	· 绑卡打 api.stripe.com + chatgpt.com
//
// 让它们共用同一个出口不一定最优，也不一定安全 —— 所以分开配。
type Stage string

const (
	// StageLink 提链阶段
	StageLink Stage = "link"
	// StageBind 绑卡阶段
	StageBind Stage = "bind"
	// StagePrepare 支付方式准备（拿 SetupIntent）
	StagePrepare Stage = "prepare"
)

// AllStages 是全部阶段，顺序即执行顺序。
var AllStages = []Stage{StageLink, StagePrepare, StageBind}

// Label 返回中文名，用于前端展示。
func (s Stage) Label() string {
	switch s {
	case StageLink:
		return "提链"
	case StageBind:
		return "绑卡"
	case StagePrepare:
		return "支付准备"
	}
	return string(s)
}

// StageConfig 是一个阶段的出口配置。
type StageConfig struct {
	// Proxy 出口代理。留空表示沿用上一阶段或直连。
	// 支持 env: / file: 引用。
	Proxy string `yaml:"proxy" json:"proxy"`
	// Egress 期望的出口国家（ISO 3166-1 alpha-2），用于取代理后校验。
	Egress string `yaml:"egress" json:"egress"`
	// Country 该阶段期望的建单国家。留空表示用 defaults。
	Country string `yaml:"country" json:"country"`
	// FailFast 出口不符时立即失败（true）还是继续（false）。
	FailFast bool `yaml:"fail_fast" json:"fail_fast"`
	// Note 说明，前端展示用
	Note string `yaml:"note" json:"note"`
}

// StagesCfg 是全部阶段的配置。
type StagesCfg struct {
	Link    StageConfig `yaml:"link" json:"link"`
	Prepare StageConfig `yaml:"prepare" json:"prepare"`
	Bind    StageConfig `yaml:"bind" json:"bind"`
}

// Get 按阶段取配置。
func (sc StagesCfg) Get(s Stage) StageConfig {
	switch s {
	case StageLink:
		return sc.Link
	case StageBind:
		return sc.Bind
	case StagePrepare:
		return sc.Prepare
	}
	return StageConfig{}
}

// ResolvedProxy 返回解析后的代理地址（处理 env: / file: 引用）。
func (sc StageConfig) ResolvedProxy() string {
	return ResolveSecret(sc.Proxy)
}

// Ready 判断该阶段是否配了独立出口。
func (sc StageConfig) Ready() bool {
	return strings.TrimSpace(sc.Proxy) != ""
}

// ---------------------------------------------------------------- 阶段切换

// stageState 记录当前生效的阶段，用来避免重复切换。
type stageState struct {
	mu      sync.RWMutex
	current Stage
	proxies map[Stage]string
}

var globalStage = &stageState{proxies: map[Stage]string{}}

// ResetStages 清空阶段缓存（配置热更新后调用）。
func ResetStages() {
	globalStage.mu.Lock()
	defer globalStage.mu.Unlock()
	globalStage.current = ""
	globalStage.proxies = map[Stage]string{}
	CloseProxyIdleConnections()
	CloseCloakIdleConnections()
}

// SwitchStage 把会话切到指定阶段的出口。
//
// 返回该阶段实际使用的代理（空表示直连）。已经在该阶段时不重复切换。
func (s *Session) SwitchStage(st Stage, cfg StagesCfg) (string, error) {
	sc := cfg.Get(st)
	wantProxy := sc.ResolvedProxy()

	globalStage.mu.RLock()
	already := globalStage.current == st && globalStage.proxies[st] == wantProxy
	globalStage.mu.RUnlock()
	if already {
		return wantProxy, nil
	}

	// 换 transport
	var spec ProxySpec
	if wantProxy != "" {
		parsed, err := ParseProxyURL(wantProxy)
		if err != nil {
			return "", fmt.Errorf("阶段 %s 的代理地址无效: %w", st.Label(), err)
		}
		spec = parsed
	}
	s.Proxy = spec

	useH2 := s.ALPN == ALPNH2First
	fp := s.Fingerprint
	if fp == "" {
		fp = DefaultCloakPreset
	}
	if spec.IsDirect() {
		s.client.Transport = cachedCloakTransport(fp, useH2)
	} else {
		s.client.Transport = cachedCloakProxyTransport(fp, useH2, spec)
	}

	globalStage.mu.Lock()
	globalStage.current = st
	globalStage.proxies[st] = wantProxy
	globalStage.mu.Unlock()

	return wantProxy, nil
}

// CurrentStage 返回当前阶段（全局，仅供诊断）。
func CurrentStage() Stage {
	globalStage.mu.RLock()
	defer globalStage.mu.RUnlock()
	return globalStage.current
}

// ---------------------------------------------------------------- 出口预检

// StageCheckResult 是一次阶段出口检查的结果。
type StageCheckResult struct {
	Stage    string `json:"stage"`
	Label    string `json:"label"`
	Proxy    string `json:"proxy"`    // 脱敏
	Expected string `json:"expected"` // 期望国家
	Actual   string `json:"actual"`   // 实际国家
	IP       string `json:"ip"`
	ISP      string `json:"isp"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// CheckStages 依次切到每个阶段并探测出口，返回结果列表。
//
// 给前端「一键体检」用。不会发任何业务请求，只打 ipinfo.io。
func (s *Session) CheckStages(cfg StagesCfg) []StageCheckResult {
	out := make([]StageCheckResult, 0, len(AllStages))

	for _, st := range AllStages {
		sc := cfg.Get(st)
		res := StageCheckResult{
			Stage:    string(st),
			Label:    st.Label(),
			Expected: strings.ToUpper(sc.Egress),
		}

		proxyUsed, err := s.SwitchStage(st, cfg)
		if err != nil {
			res.Message = err.Error()
			out = append(out, res)
			continue
		}
		if proxyUsed == "" {
			res.Proxy = "(直连)"
		} else {
			res.Proxy = ProxySpec{URL: proxyUsed}.String()
		}

		info, err := s.ProbeEgress()
		if err != nil {
			res.Message = "探测失败: " + err.Error()
			out = append(out, res)
			continue
		}
		res.IP = info.IP
		res.ISP = info.ISP
		res.Actual = info.Country

		if res.Expected == "" || res.Actual == res.Expected {
			res.OK = true
			res.Message = "出口匹配"
		} else {
			res.Message = fmt.Sprintf("出口 %s ≠ 期望 %s", res.Actual, res.Expected)
		}
		out = append(out, res)
	}
	return out
}

// ---------------------------------------------------------------- 环境变量兼容

// StagesFromEnv 从 Python 侧沿用的环境变量读阶段配置。
//
// 兼容这些：
//
//	REG_FACTORY_PLUS_LINK_PROXY
//	REG_FACTORY_PLUS_BIND_PROXY
//	REG_FACTORY_PLUS_PREPARE_PROXY
//	REG_FACTORY_PLUS_LINK_EGRESS
//	REG_FACTORY_PLUS_BIND_EGRESS
//	REG_FACTORY_PLUS_PREPARE_EGRESS
//
// 配置文件里的值优先，环境变量只做兜底。
func StagesFromEnv() StagesCfg {
	env := settings.NewEnv(settings.Load().EnvFile)
	get := func(suffix string) string {
		return strings.TrimSpace(env.Effective("REG_FACTORY_PLUS_" + suffix))
	}
	return StagesCfg{
		Link: StageConfig{
			Proxy:  get("LINK_PROXY"),
			Egress: get("LINK_EGRESS"),
		},
		Prepare: StageConfig{
			Proxy:  get("PREPARE_PROXY"),
			Egress: get("PREPARE_EGRESS"),
		},
		Bind: StageConfig{
			Proxy:  get("BIND_PROXY"),
			Egress: get("BIND_EGRESS"),
		},
	}
}

// MergeStages 把配置文件和环境变量的阶段配置合并。
//
// 规则：配置文件有值就用配置，否则用环境变量。这样已有部署不用改。
func MergeStages(fromFile StagesCfg) StagesCfg {
	env := StagesFromEnv()
	merged := fromFile

	mergeOne := func(dst *StageConfig, src StageConfig) {
		if strings.TrimSpace(dst.Proxy) == "" {
			dst.Proxy = src.Proxy
		}
		if strings.TrimSpace(dst.Egress) == "" {
			dst.Egress = src.Egress
		}
	}
	mergeOne(&merged.Link, env.Link)
	mergeOne(&merged.Prepare, env.Prepare)
	mergeOne(&merged.Bind, env.Bind)

	// 未单独配 egress 时，用建单国家兜底
	if merged.Link.Egress == "" {
		merged.Link.Egress = merged.Link.Country
	}
	if merged.Bind.Egress == "" {
		merged.Bind.Egress = merged.Bind.Country
	}
	return merged
}
