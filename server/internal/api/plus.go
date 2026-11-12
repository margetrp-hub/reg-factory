package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/plus"
)

// plusRegions 返回可选的建单区域预设，供 UI 下拉。
//
// GET /api/plus/regions
//
// 返回
//
//	{
//	  "regions": [ {id,label,country,currency,egress,note,enabled}, ... ],
//	  "default": {country,currency,bind_country,bind_currency,promo_campaign,...}
//	}
func (s *Server) plusRegions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"regions": plus.Regions(),
		"default": plus.DefaultConfig(),
	})
}

// plusRegionCheck 校验某个区域预设与指定出口是否匹配。
//
// GET /api/plus/regions/check?region=ph-low&egress=SG
//
// 用于 UI 在用户选完区域、选完代理后即时提示，
// 避免发请求到 OpenAI 才拿到 400 Billing country must match。
func (s *Server) plusRegionCheck(c *gin.Context) {
	id := c.Query("region")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 region 参数"})
		return
	}
	region, ok := plus.FindRegion(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "区域不存在: " + id})
		return
	}

	egress := c.Query("egress")
	msg := region.EgressMismatch(egress)

	resp := gin.H{
		"region":  region,
		"egress":  egress,
		"ok":      msg == "",
		"message": msg,
	}
	if msg == "" {
		// 匹配时顺带给出落到 Config 的最终值，UI 可以直接展示
		resp["config"] = region.ToConfig(plus.DefaultConfig())
	}
	c.JSON(http.StatusOK, resp)
}

// plusRegionsReload 强制重载区域预设（预留给面板「刷新配置」用）。
//
// POST /api/plus/regions/reload
//
// 当前预设是编译期常量，这个端点返回当前值，方便前端做幂等同步。
func (s *Server) plusRegionsReload(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"regions":  plus.Regions(),
		"reloaded": true,
	})
}
