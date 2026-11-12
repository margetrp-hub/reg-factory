package egress

import (
	"net/http"
	"strings"
	"sync"

	"github.com/sardanioss/httpcloak/fingerprint"
)

// CloakPresets 是 httpcloak 内置的指纹预设名。
//
// 与 Python curl_cffi 的 impersonate 参数不同，httpcloak 用版本号命名。
// 完整列表见 fingerprint 包；这里列出常用的几个便于配置校验。
var CloakPresets = []string{
	"chrome-latest",
	"chrome-146",
	"chrome-143",
	"chrome-143-android",
	"chrome-142-windows",
	"chrome-142-linux",
	"chrome-141",
	"chrome-133",
	"chrome-133-psk",
}

// DefaultCloakPreset 是默认预设。
//
// chrome-latest 会解析到当前最新版本（v1.7.3 是 chrome-146），
// 上游更新后自动跟随，不用改代码。
const DefaultCloakPreset = "chrome-latest"

// CloakPresetValid 检查预设名是否被 httpcloak 认识。
//
// fingerprint.GetStrict 对未知名字返回 nil；fingerprint.Get 则会
// 静默回退到 chrome-146。启动自检应该用 Strict，避免拼错名却没人发现。
func CloakPresetValid(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	return fingerprint.GetStrict(n) != nil
}

// newCloakTransport 构造 httpcloak 的 transport。
//
// useH2 决定用 HTTP/2 还是 HTTP/1.1 帧层。两者都实现了 http.RoundTripper，
// 可以直接塞进 http.Client.Transport。
//
// httpcloak 覆盖的范围比手搓 utls 大得多：
//   - TLS      JA3/JA4、GREASE、X25519MLKEM768、ECH
//   - HTTP/2   SETTINGS 帧、WINDOW_UPDATE、HPACK 流优先级
//   - 头部     Sec-Fetch-*、Client Hints、头部顺序
//   - TCP/IP   TTL / MSS / Window
func newCloakTransport(presetName string, useH2 bool) http.RoundTripper {
	return newCloakRoundTripper(presetName, useH2)
}

var (
	cloakCacheMu sync.Mutex
	cloakCache   = map[string]http.RoundTripper{}
)

// cachedCloakTransport 按 (preset, h1/h2) 分桶缓存 transport。
func cachedCloakTransport(presetName string, useH2 bool) http.RoundTripper {
	key := strings.ToLower(strings.TrimSpace(presetName)) + "|h2=" + boolStr(useH2)
	cloakCacheMu.Lock()
	defer cloakCacheMu.Unlock()
	if tr, ok := cloakCache[key]; ok {
		return tr
	}
	tr := newCloakTransport(presetName, useH2)
	cloakCache[key] = tr
	return tr
}

// CloseCloakIdleConnections 释放 httpcloak 缓存的空闲连接。
func CloseCloakIdleConnections() {
	cloakCacheMu.Lock()
	defer cloakCacheMu.Unlock()
	for _, tr := range cloakCache {
		if c, ok := tr.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
	cloakCache = map[string]http.RoundTripper{}
}

// CloakInfo 返回某个预设的指纹描述，供自检和排障使用。
//
// fingerprint.Describe 会输出该预设的 TLS/HTTP2/头部参数摘要。
func CloakInfo(presetName string) (string, error) {
	return fingerprint.Describe(presetName)
}

// transportForCloak 是给 Session 用的统一入口：决定走 httpcloak 还是
// 回退到内置 utls 实现。
//
// 选择逻辑：
//   - cfg.UseCloak 为 true（默认）且预设名合法 → httpcloak
//   - 否则 → 内置 utls（只有 TLS 层，SETTINGS 和头部顺序不覆盖）
func transportForCloak(fingerprintName string, alpn ALPNMode, useCloak bool) http.RoundTripper {
	if !useCloak {
		return transportFor(fingerprintName, alpn)
	}
	preset := strings.TrimSpace(fingerprintName)
	if preset == "" {
		preset = DefaultCloakPreset
	}
	if !CloakPresetValid(preset) {
		// 名字不对就退回内置实现，让请求仍能发出，但日志里要能看到
		return transportFor(fingerprintName, alpn)
	}
	return cachedCloakTransport(preset, alpn == ALPNH2First)
}
