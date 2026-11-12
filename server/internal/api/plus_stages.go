package api

import (
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/tiantianGPU/reg-factory/server/internal/plus"
)

// plusStagesGet 返回分阶段出口配置。
//
// GET /api/plus/stages
//
// 返回
//
//	{
//	  "stages": {
//	    "link":    {proxy, egress, country, fail_fast, note},
//	    "prepare": {...},
//	    "bind":    {...}
//	  },
//	  "labels": {"link": "提链", "prepare": "支付准备", "bind": "绑卡"},
//	  "config_path": "/data/config/plus.yaml"
//	}
func (s *Server) plusStagesGet(c *gin.Context) {
	cf := plus.CurrentConfig()
	if cf == nil {
		var err error
		cf, err = plus.LoadConfigFile(plus.DefaultConfigPath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	stages := plus.MergeStages(cf.Stages)
	labels := gin.H{}
	for _, st := range plus.AllStages {
		labels[string(st)] = st.Label()
	}

	// 代理地址脱敏后再给前端
	c.JSON(http.StatusOK, gin.H{
		"stages":      maskStages(stages),
		"labels":      labels,
		"config_path": plus.DefaultConfigPath,
	})
}

// plusStagesPut 更新分阶段出口配置。
//
// PUT /api/plus/stages
// Body: {"link": {...}, "prepare": {...}, "bind": {...}}
//
// 只更新 minus 的 stage 字段，其余配置原样保留。
func (s *Server) plusStagesPut(c *gin.Context) {
	var incoming plus.StagesCfg
	if err := c.ShouldBindJSON(&incoming); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体解析失败: " + err.Error()})
		return
	}

	// 读现有配置
	raw, err := os.ReadFile(plus.DefaultConfigPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读配置: " + err.Error()})
		return
	}
	var full map[string]any
	if err := yaml.Unmarshal(raw, &full); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "解析配置: " + err.Error()})
		return
	}

	// 只替换 stages 段
	full["stages"] = map[string]any{
		"link": map[string]any{
			"proxy":     incoming.Link.Proxy,
			"egress":    incoming.Link.Egress,
			"country":   incoming.Link.Country,
			"fail_fast": incoming.Link.FailFast,
			"note":      incoming.Link.Note,
		},
		"prepare": map[string]any{
			"proxy":     incoming.Prepare.Proxy,
			"egress":    incoming.Prepare.Egress,
			"country":   incoming.Prepare.Country,
			"fail_fast": incoming.Prepare.FailFast,
			"note":      incoming.Prepare.Note,
		},
		"bind": map[string]any{
			"proxy":     incoming.Bind.Proxy,
			"egress":    incoming.Bind.Egress,
			"country":   incoming.Bind.Country,
			"fail_fast": incoming.Bind.FailFast,
			"note":      incoming.Bind.Note,
		},
	}

	out, err := yaml.Marshal(full)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化: " + err.Error()})
		return
	}
	if err := os.WriteFile(plus.DefaultConfigPath, out, 0o600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写配置: " + err.Error()})
		return
	}

	// 热重载
	if _, err := plus.ReloadConfig(); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"warning": "已写入但重载失败: " + err.Error(),
		})
		return
	}
	plus.ResetStages()

	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "已保存并生效"})
}

// plusStagesCheck 对每个阶段做出口体检。
//
// POST /api/plus/stages/check
// Body: {"access_token": "...", "account_id": "..."}  ← 可选，用于验证能连通
//
// 只打 ipinfo.io，不发任何业务请求。零风险。
func (s *Server) plusStagesCheck(c *gin.Context) {
	var req struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	_ = c.ShouldBindJSON(&req)

	cf := plus.CurrentConfig()
	if cf == nil {
		var err error
		cf, err = plus.LoadConfigFile(plus.DefaultConfigPath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	cfg := plus.DefaultConfig()
	cfg.TLSImpersonate = cf.Defaults.TLSImpersonate
	cfg.ALPN = cf.Defaults.ALPN
	if cfg.TLSImpersonate == "" {
		cfg.TLSImpersonate = plus.DefaultCloakPreset
	}
	if cfg.ALPN == "" {
		cfg.ALPN = "h1"
	}

	sess := plus.NewSessionWithConfig(req.AccessToken, req.AccountID, cfg)
	results := sess.CheckStages(plus.MergeStages(cf.Stages))

	allOK := true
	for _, r := range results {
		if !r.OK {
			allOK = false
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":      allOK,
		"results": results,
	})
}

// maskStages 脱敏代理地址，只保留 scheme + host。
func maskStages(sc plus.StagesCfg) map[string]any {
	mask := func(sc plus.StageConfig) gin.H {
		p := sc.Proxy
		if p != "" && sc.ResolvedProxy() == "" {
			// env/file 引用解析不到时明确标出
			p = p + "  (未解析)"
		}
		return gin.H{
			"proxy":     p,
			"egress":    sc.Egress,
			"country":   sc.Country,
			"fail_fast": sc.FailFast,
			"note":      sc.Note,
		}
	}
	return map[string]any{
		"link":    mask(sc.Link),
		"prepare": mask(sc.Prepare),
		"bind":    mask(sc.Bind),
	}
}
