package plus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 为什么需要预热
// ------------
// 直接拿一个刚切换的代理去打 /backend-api/payments/checkout，会同时暴露：
//   · 一个陌生的 TLS 会话
//   · 没有任何 cookie 的首次访问
//   · 从没在这个 IP 上出现过任何浏览行为
//
// 这三条叠加会被 Cloudflare / OpenAI Radar 当成机器流量。
// 预热就是在正式请求前，先做一轮低速的自然访问，把这三条补上：
//   1. GET / 首页        建立 TLS 会话 + 拿 __cf_bm 等基础 cookie
//   2. GET /api/auth/session  确认这个出口能看到当前账号的会话
//   3. 间隔几百毫秒，模拟真人浏览节奏
//
// 预热失败不代表正式请求一定失败，但失败率高说明这个出口不干净。

// WarmupResult 是一次预热的结果。
type WarmupResult struct {
	OK          bool   `json:"ok"`
	Proxy       string `json:"proxy"` // 脱敏
	Egress      string `json:"egress,omitempty"`
	IP          string `json:"ip,omitempty"`
	ISP         string `json:"isp,omitempty"`
	HomeStatus  int    `json:"home_status"`
	AuthStatus  int    `json:"auth_status"`
	Cookies     int    `json:"cookies"`
	CfClearance bool   `json:"cf_clearance"`
	LatencyMs   int64  `json:"latency_ms"`
	Message     string `json:"message,omitempty"`
}

// Warmup 在正式请求前建立会话。
//
// 步骤：
//  1. 探测出口（同时拿到 IP / 国家 / ISP）
//  2. GET https://chatgpt.com/
//  3. GET https://chatgpt.com/api/auth/session
//
// 只发 GET，不碰任何写接口，零副作用。
func (s *Session) Warmup(ctx context.Context, cfg Config) (*WarmupResult, error) {
	started := time.Now()
	res := &WarmupResult{}

	if !s.Proxy.IsDirect() {
		res.Proxy = s.Proxy.String()
	} else {
		res.Proxy = "(直连)"
	}

	// 步骤 1：出口探测
	info, err := s.ProbeEgress()
	if err != nil {
		res.Message = "出口探测失败: " + err.Error()
		return res, fmt.Errorf("预热: %w", err)
	}
	res.IP = info.IP
	res.Egress = info.Country
	res.ISP = info.ISP

	// 步骤 2：首页
	homeStatus, err := s.warmGet(ctx, cfg, AppBase+"/",
		"/", AppBase+"/")
	if err != nil {
		res.Message = "首页失败: " + err.Error()
		return res, err
	}
	res.HomeStatus = homeStatus

	// 真人节奏：首页和 session 之间停一下
	time.Sleep(time.Duration(300+seededIntn(500)) * time.Millisecond)

	// 步骤 3：auth session（确认这个出口能认出账号）
	authStatus, err := s.warmGet(ctx, cfg,
		AppBase+"/api/auth/session", "/api/auth/session", AppBase+"/")
	if err != nil {
		// session 接口失败不阻断预热，但记录下来
		res.Message = "session 接口异常: " + err.Error()
	}
	res.AuthStatus = authStatus

	// 统计 cookie
	res.Cookies = s.CookieCount()
	res.CfClearance = s.HasCookie("cf_clearance")
	res.LatencyMs = time.Since(started).Milliseconds()

	// 判据：首页 200 或 403（CF 挑战也算通了，说明 IP 可达）
	res.OK = homeStatus == 200 || homeStatus == 403

	if !res.OK {
		res.Message = fmt.Sprintf("首页返回 %d", homeStatus)
	}
	return res, nil
}

// WarmupStage 切到某个阶段，验证出口，然后预热。
//
// 这是「准备一个阶段」的完整动作：
//  1. SwitchStage   换 transport
//  2. ProbeEgress   验证出口国家
//  3. Warmup        建会话
//
// fail_fast=true 时，出口不匹配直接返回错误，不继续预热。
func (s *Session) WarmupStage(
	ctx context.Context, st Stage, cfg StagesCfg, base Config,
) (*WarmupResult, error) {
	sc := cfg.Get(st)

	// 步骤 1：切 transport
	proxyUsed, err := s.SwitchStage(st, cfg)
	if err != nil {
		return &WarmupResult{OK: false, Message: err.Error()}, err
	}

	// 步骤 2：验证出口
	info, err := s.ProbeEgress()
	if err != nil {
		return &WarmupResult{OK: false, Proxy: proxyUsed,
			Message: "出口探测失败: " + err.Error()}, err
	}

	want := strings.ToUpper(strings.TrimSpace(sc.Egress))
	if want != "" && info.Country != want {
		msg := fmt.Sprintf("阶段 %s 出口 %s(%s) ≠ 期望 %s",
			st.Label(), info.Country, info.IP, want)
		if sc.FailFast {
			return &WarmupResult{
				OK: false, Proxy: proxyUsed, Egress: info.Country,
				IP: info.IP, ISP: info.ISP, Message: msg,
			}, fmt.Errorf("%s", msg)
		}
		// 不 fail-fast 时只记录警告，继续预热
	}

	// 步骤 3：预热
	res, werr := s.Warmup(ctx, base)
	if res != nil {
		res.Proxy = proxyUsed
		if res.Proxy == "" {
			res.Proxy = "(直连)"
		}
		if res.Egress == "" {
			res.Egress = info.Country
			res.IP = info.IP
			res.ISP = info.ISP
		}
	}
	return res, werr
}

// warmGet 发一个预热用的 GET，返回状态码。
//
// 与业务请求的区别：
//   - 不要求 2xx（403 的 CF 挑战也算「IP 可达」）
//   - 带浏览器导航头（Sec-Fetch-*），更像真人点进来
func (s *Session) warmGet(
	ctx context.Context, cfg Config, rawURL, route, referer string,
) (int, error) {
	timeout := timeoutOf(cfg)
	if timeout > 60 {
		timeout = 60
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}

	for k, v := range s.headers(route) {
		if k == "Content-Type" {
			continue // GET 不带
		}
		req.Header.Set(k, v)
	}
	// 导航语义：从地址栏直接进来，不是 XHR
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Referer", referer)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 丢弃响应体，只留状态码
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))

	s.captureUpdates(resp)
	s.absorbCookies(resp)

	return resp.StatusCode, nil
}

// ---------------------------------------------------------------- cookie 跟踪

// absorbCookies 把响应里的 Set-Cookie 记进会话。
//
// httpcloak 的 transport 自己有 cookie jar，但我们的 Session 是包在
// net/http 外面的，所以要自己维护一份可观测的计数。
func (s *Session) absorbCookies(resp *http.Response) {
	if resp == nil {
		return
	}
	for _, ck := range resp.Cookies() {
		if ck == nil || ck.Name == "" {
			continue
		}
		s.cookieMu.Lock()
		if s.cookies == nil {
			s.cookies = map[string]string{}
		}
		s.cookies[ck.Name] = ck.Value
		s.cookieMu.Unlock()

		// 同步回 net/http 的 Jar（如果配了）
		if s.client != nil && s.client.Jar != nil {
			u := resp.Request.URL
			if u != nil {
				s.client.Jar.SetCookies(u, []*http.Cookie{ck})
			}
		}
	}
}

// CookieCount 返回已知 cookie 数量。
func (s *Session) CookieCount() int {
	s.cookieMu.RLock()
	defer s.cookieMu.RUnlock()
	return len(s.cookies)
}

// HasCookie 判断某个 cookie 是否存在且非空。
func (s *Session) HasCookie(name string) bool {
	s.cookieMu.RLock()
	defer s.cookieMu.RUnlock()
	v, ok := s.cookies[name]
	return ok && strings.TrimSpace(v) != ""
}

// Cookies 返回 cookie 快照（脱敏：只返回名字）。
func (s *Session) CookieNames() []string {
	s.cookieMu.RLock()
	defer s.cookieMu.RUnlock()
	out := make([]string, 0, len(s.cookies))
	for k := range s.cookies {
		out = append(out, k)
	}
	return out
}

func seededIntn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(time.Now().UnixNano()>>3) % n
}
