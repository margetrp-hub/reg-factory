// Package captcha — 2captcha 打码服务 adapter。
//
// 为什么单独写
// ------------
// 项目里已有 CapSolver / EZCaptcha 的 Python solver，但没有 Go 版。
// 2captcha 的 API 最简单（纯 HTTP in.php / res.php），而且是主流的
// FunCaptcha（Arkose）和 PressAndHold（PerimeterX）供应商。
//
// 支持的 captcha 类型
// -------------------
//
//	funcaptcha     Arkose Labs / FunCaptcha —— Microsoft 注册页 enforcementFrame
//	hcaptcha       hCaptcha
//	recaptcha_v2   Google reCAPTCHA v2
//	pressandhold   PerimeterX 按住不放（Outlook 注册会遇到）
//	turnstile      Cloudflare Turnstile
//	image          图片验证码
//
// API
// ---
//
//	POST https://2captcha.com/in.php
//	  key, method, ...参数, json=1
//	  → {"status":1,"request":"<taskid>"}
//
//	GET  https://2captcha.com/res.php?key=&action=get&id=&json=1
//	  → {"status":1,"request":"<token>"}         成功
//	  → {"status":0,"request":"CAPCHA_NOT_READY"} 还没好
//	  → {"status":0,"request":"ERROR_XXX"}        失败
//
// 用法
// ----
//
//	c := captcha.New("")  // 空则从 TWOCAPTCHA_API_KEY 读
//	token, err := c.SolveFuncaptcha(ctx,
//	    "B7D8911C-5CC8-A9A3-35B0-554ACEE604DA",
//	    "https://signup.live.com/signup?lic=1",
//	    "https://arkoselabs.com")
package captcha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	InURL  = "https://2captcha.com/in.php"
	ResURL = "https://2captcha.com/res.php"
)

// pending 是「还没好」的响应。
var pending = map[string]bool{
	"CAPCHA_NOT_READY":  true,
	"CAPTCHA_NOT_READY": true,
}

// ErrPending 表示任务还在处理中（内部用，不会返回给调用方）。
var ErrPending = errors.New("captcha: pending")

// ErrNoKey 表示没配 API key。
var ErrNoKey = errors.New("captcha: TWOCAPTCHA_API_KEY 未配置或长度不为 32")

// Error 是打码失败。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("captcha: %s (%s)", e.Code, e.Msg)
	}
	return "captcha: " + e.Code
}

// Client 是 2captcha 客户端。
//
// 零值不可用，必须用 New 构造。
type Client struct {
	key  string
	http *http.Client

	// PollInterval 是轮询间隔，默认 5s。
	PollInterval time.Duration
	// DefaultTimeout 是单次 Solve 的总超时，默认 180s。
	DefaultTimeout time.Duration
}

// New 构造客户端。key 为空时从 TWOCAPTCHA_API_KEY 读。
func New(key string) *Client {
	k := strings.TrimSpace(key)
	if k == "" {
		k = strings.TrimSpace(os.Getenv("TWOCAPTCHA_API_KEY"))
	}
	return &Client{
		key:            k,
		http:           &http.Client{Timeout: 40 * time.Second},
		PollInterval:   5 * time.Second,
		DefaultTimeout: 180 * time.Second,
	}
}

// Key 返回当前 key（脱敏）。
func (c *Client) Key() string {
	if len(c.key) != 32 {
		return ""
	}
	return c.key[:4] + "***" + c.key[len(c.key)-4:]
}

// usable 校验 key。
func (c *Client) usable() error {
	if len(c.key) != 32 {
		return ErrNoKey
	}
	return nil
}

// ---------------------------------------------------------------- 底层

type inResp struct {
	Status  int    `json:"status"`
	Request string `json:"request"`
	Error   string `json:"error_text"`
}

// create 提交任务，返回 task id。
func (c *Client) create(ctx context.Context, payload map[string]string) (string, error) {
	if err := c.usable(); err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("key", c.key)
	form.Set("json", "1")
	for k, v := range payload {
		if v != "" {
			form.Set(k, v)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, InURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("captcha: 提交失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var r inResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("captcha: 响应不是 JSON: %s", truncate(string(raw), 200))
	}
	if r.Status != 1 {
		return "", &Error{Code: r.Request, Msg: r.Error}
	}
	return r.Request, nil
}

// poll 轮询结果。
func (c *Client) poll(ctx context.Context, taskID string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = c.DefaultTimeout
	}
	deadline := time.Now().Add(timeout)
	interval := c.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for {
		if time.Now().After(deadline) {
			return "", &Error{Code: "TIMEOUT", Msg: fmt.Sprintf("%s 内没拿到结果", timeout)}
		}

		token, err := c.fetch(ctx, taskID)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, ErrPending) {
			return "", err
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (c *Client) fetch(ctx context.Context, taskID string) (string, error) {
	q := url.Values{}
	q.Set("key", c.key)
	q.Set("action", "get")
	q.Set("id", taskID)
	q.Set("json", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ResURL+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("captcha: 轮询失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var r inResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("captcha: 轮询响应不是 JSON: %s", truncate(string(raw), 200))
	}

	if r.Status == 1 {
		return r.Request, nil
	}
	if pending[r.Request] {
		return "", ErrPending
	}
	return "", &Error{Code: r.Request, Msg: r.Error}
}

// solve 是 create + poll 的组合。
func (c *Client) solve(ctx context.Context, payload map[string]string, timeout time.Duration) (string, error) {
	id, err := c.create(ctx, payload)
	if err != nil {
		return "", err
	}
	return c.poll(ctx, id, timeout)
}

// ---------------------------------------------------------------- 公开 API

// Balance 查余额（美元）。
func (c *Client) Balance(ctx context.Context) (float64, error) {
	if err := c.usable(); err != nil {
		return 0, err
	}
	q := url.Values{}
	q.Set("key", c.key)
	q.Set("action", "getbalance")
	q.Set("json", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ResURL+"?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	// 2captcha 的 request 字段类型不稳定：
	//   {"status":1,"request":"2.88362"}   ← 字符串（实测）
	//   {"status":1,"request":2.88362}     ← 也可能是数字
	// 所以用 RawMessage 再手工转。
	var r struct {
		Status  int             `json:"status"`
		Request json.RawMessage `json:"request"`
		Error   string          `json:"error_text"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, fmt.Errorf("captcha: 余额响应无法解析: %s", truncate(string(raw), 120))
	}
	if r.Status != 1 {
		return 0, &Error{Code: "BALANCE", Msg: r.Error}
	}

	// 先当字符串去引号，再 ParseFloat
	s := strings.Trim(string(r.Request), `"`)
	if v, e := strconv.ParseFloat(s, 64); e == nil {
		return v, nil
	}
	return 0, fmt.Errorf("captcha: 余额不是数字: %s", truncate(string(r.Request), 80))
}

// SolveFuncaptcha 解 Arkose Labs / FunCaptcha。
//
// sitekey 是 Arkose 的 public key，surl 是 Arkose 的服务地址
// （Microsoft 注册页一般是 https://arkoselabs.com）。
func (c *Client) SolveFuncaptcha(ctx context.Context, sitekey, pageurl, surl string) (string, error) {
	p := map[string]string{
		"method":    "funcaptcha",
		"publickey": sitekey,
		"pageurl":   pageurl,
		"surl":      surl,
	}
	return c.solve(ctx, p, 0)
}

// SolveFuncaptchaData 解带 blob 数据的 FunCaptcha（Arkose 的新版本）。
func (c *Client) SolveFuncaptchaData(ctx context.Context, sitekey, pageurl, surl, data string) (string, error) {
	p := map[string]string{
		"method":    "funcaptcha",
		"publickey": sitekey,
		"pageurl":   pageurl,
		"surl":      surl,
		"data":      data,
	}
	return c.solve(ctx, p, 0)
}

// SolveHcaptcha 解 hCaptcha。
func (c *Client) SolveHcaptcha(ctx context.Context, sitekey, pageurl string) (string, error) {
	return c.solve(ctx, map[string]string{
		"method":  "hcaptcha",
		"sitekey": sitekey,
		"pageurl": pageurl,
	}, 0)
}

// SolveRecaptchaV2 解 reCAPTCHA v2。
func (c *Client) SolveRecaptchaV2(ctx context.Context, sitekey, pageurl string) (string, error) {
	return c.solve(ctx, map[string]string{
		"method":    "userrecaptcha",
		"googlekey": sitekey,
		"pageurl":   pageurl,
	}, 0)
}

// SolveTurnstile 解 Cloudflare Turnstile。
//
// action 可选（Turnstile widget 的 action 值）。
func (c *Client) SolveTurnstile(ctx context.Context, sitekey, pageurl, action string) (string, error) {
	return c.solve(ctx, map[string]string{
		"method":  "turnstile",
		"sitekey": sitekey,
		"pageurl": pageurl,
		"action":  action,
	}, 0)
}

// SolvePressAndHold 解 PerimeterX 的 press-and-hold。
//
// body 是 PX 的 challenge 参数（从页面里抓的），可以为空。
func (c *Client) SolvePressAndHold(ctx context.Context, pageurl, body string) (string, error) {
	p := map[string]string{
		"method":  "pressandhold",
		"pageurl": pageurl,
	}
	if body != "" {
		p["body"] = body
	}
	return c.solve(ctx, p, 0)
}

// SolveImage 解图片验证码。返回识别出的文本。
func (c *Client) SolveImage(ctx context.Context, imageBytes []byte) (string, error) {
	if len(imageBytes) == 0 {
		return "", errors.New("captcha: 图片为空")
	}
	if err := c.usable(); err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("key", c.key)
	form.Set("method", "base64")
	form.Set("body", base64.StdEncoding.EncodeToString(imageBytes))
	form.Set("json", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, InURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("captcha: 提交图片失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var r inResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("captcha: 响应不是 JSON: %s", truncate(string(raw), 200))
	}
	if r.Status != 1 {
		return "", &Error{Code: r.Request, Msg: r.Error}
	}
	return c.poll(ctx, r.Request, 120*time.Second)
}

// ---------------------------------------------------------------- 辅助

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ReportBad 上报错误结果（让 2captcha 退钱）。可选调用。
func (c *Client) ReportBad(ctx context.Context, taskID string) error {
	if err := c.usable(); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("key", c.key)
	q.Set("action", "reportbad")
	q.Set("id", taskID)
	q.Set("json", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ResURL+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
