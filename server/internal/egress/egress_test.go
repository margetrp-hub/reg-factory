package egress

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- ParseProxyURL

func TestParseProxyURL(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantUser string
		wantPass string
		wantErr  bool
	}{
		{
			name:     "标准 http + 账密",
			raw:      "http://user:pass@us.cliproxy.io:3010",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:     "无 scheme（自动补 http）",
			raw:      "user:pass@us.cliproxy.io:3010",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:     "socks5",
			raw:      "socks5://user:pw@127.0.0.1:1080",
			wantUser: "user",
			wantPass: "pw",
		},
		{
			name:     "只有用户名",
			raw:      "http://onlyuser@host:8080",
			wantUser: "onlyuser",
			wantPass: "",
		},
		{
			name: "无账密",
			raw:  "http://host:8080",
		},
		{
			name:    "空字符串",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "空白字符",
			raw:     "   \t\n  ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseProxyURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应该报错, 实际 spec=%+v", spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if spec.Username != tc.wantUser {
				t.Errorf("Username=%q, 期望 %q", spec.Username, tc.wantUser)
			}
			if spec.Password != tc.wantPass {
				t.Errorf("Password=%q, 期望 %q", spec.Password, tc.wantPass)
			}
			if spec.URL == "" {
				t.Error("URL 不应为空")
			}
		})
	}
}

func TestParseProxyURLPreservesRaw(t *testing.T) {
	// 无 scheme 时会补 http://，URL 字段应该是补过的完整地址
	spec, err := ParseProxyURL("1.2.3.4:8080")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !strings.HasPrefix(spec.URL, "http://") {
		t.Errorf("应该补上 http:// 前缀, 实际 %q", spec.URL)
	}
}

// ---------------------------------------------------------------- EvaluatePurity

func TestEvaluatePurityNilInfo(t *testing.T) {
	s := EvaluatePurity(context.Background(), nil)
	if s == nil {
		t.Fatal("不该返回 nil")
	}
	if s.Grade != "F" {
		t.Errorf("nil info 应该 F 级, 实际 %s", s.Grade)
	}
	if len(s.Reasons) == 0 {
		t.Error("应该有拒绝理由")
	}
}

func TestEvaluatePurityEmptyIP(t *testing.T) {
	s := EvaluatePurity(context.Background(), &EgressInfo{})
	if s.Grade != "F" {
		t.Errorf("空 IP 应该 F 级, 实际 %s", s.Grade)
	}
}

// TestEvaluatePurityDatacenterISP 用已知的机房 ISP 名字（不走网络）。
func cleanProbe(_ context.Context, ip string) (*ipAPIResult, error) {
	return &ipAPIResult{Status: "success", Query: ip}, nil
}

func noBlacklists(context.Context, string) []string { return nil }

func TestEvaluatePurityDatacenterISP(t *testing.T) {
	// IsDatacenterISP 命中时，会直接标记 Datacenter + Hosting 并扣分。
	// 用 "Amazon" 这种一定在名单里的关键字。
	s := evaluatePurity(context.Background(), &EgressInfo{
		IP:  "1.2.3.4",
		ISP: "Amazon.com, Inc.",
	}, cleanProbe, noBlacklists)
	if !s.Datacenter {
		t.Error("Amazon 应该被判为机房")
	}
	if !s.Hosting {
		t.Error("机房应该同时标记 Hosting")
	}
	// 100 - 60（机房）+ 网络探测部分不确定，所以只断言上界
	if s.Total > 40 {
		t.Errorf("机房出口分数不该高于 40, 实际 %d", s.Total)
	}
}

func TestEvaluatePurityLatencyPenalty(t *testing.T) {
	// 延迟 >3000ms 扣 15
	s := evaluatePurity(context.Background(), &EgressInfo{
		IP:        "1.2.3.4",
		ISP:       "Some Residential ISP",
		LatencyMs: 5000,
	}, cleanProbe, noBlacklists)
	found := false
	for _, r := range s.Reasons {
		if strings.Contains(r, "延迟") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("延迟超阈值应该有理由说明, 实际 reasons=%v", s.Reasons)
	}
}

func TestPurityNote(t *testing.T) {
	var nilScore *PurityScore
	if got := nilScore.Note(); got != "(未评估)" {
		t.Errorf("nil 应该返回 (未评估), 实际 %q", got)
	}

	s := &PurityScore{Grade: "A", Total: 95, Datacenter: false}
	note := s.Note()
	if !strings.Contains(note, "A") || !strings.Contains(note, "95") {
		t.Errorf("Note 应包含等级和分数, 实际 %q", note)
	}

	s2 := &PurityScore{Grade: "D", Total: 30, Datacenter: true, Blacklists: []string{"x"}}
	note2 := s2.Note()
	if !strings.Contains(note2, "机房") {
		t.Errorf("机房标记应出现在 Note 里, 实际 %q", note2)
	}
	if !strings.Contains(note2, "黑名单1") {
		t.Errorf("黑名单计数应出现在 Note 里, 实际 %q", note2)
	}
}

// ---------------------------------------------------------------- transportFor

func TestTransportForH1(t *testing.T) {
	tr := transportFor("chrome_133", ALPNH1)
	if tr == nil {
		t.Fatal("不该返回 nil")
	}
}

func TestTransportForH2First(t *testing.T) {
	tr := transportFor("chrome_133", ALPNH2First)
	if tr == nil {
		t.Fatal("不该返回 nil")
	}
}

func TestTransportForEmptyFingerprint(t *testing.T) {
	// 空指纹应该 fallback 到默认，不该 panic
	for _, alpn := range []ALPNMode{ALPNH1, ALPNH2First, ""} {
		if tr := transportFor("", alpn); tr == nil {
			t.Errorf("指纹空 + alpn=%q 不该返回 nil", alpn)
		}
	}
}

func TestTransportForCached(t *testing.T) {
	// 同参数两次调用应该拿到同一个实例（transport_cache 的语义）
	a := transportFor("chrome_133", ALPNH2First)
	b := transportFor("chrome_133", ALPNH2First)
	if a != b {
		t.Error("同参数应该命中缓存, 返回同一实例")
	}
}

func TestTransportForDifferentFingerprints(t *testing.T) {
	// 不同指纹应该是不同实例
	a := transportFor("chrome_133", ALPNH2First)
	b := transportFor("firefox_135", ALPNH2First)
	if a == b {
		t.Error("不同指纹不该共享同一个 transport")
	}
}

// ---------------------------------------------------------------- gradeOf / IsDatacenterISP

func TestGradeOf(t *testing.T) {
	cases := []struct {
		score int
		want  string
	}{
		// 实际阈值：>=85 A / >=70 B / >=50 C / >=30 D / <30 F
		{100, "A"},
		{85, "A"},
		{84, "B"},
		{70, "B"},
		{69, "C"},
		{50, "C"},
		{49, "D"},
		{30, "D"},
		{29, "F"},
		{0, "F"},
	}
	for _, tc := range cases {
		if got := gradeOf(tc.score); got != tc.want {
			t.Errorf("score=%d 应该是 %s, 实际 %s", tc.score, tc.want, got)
		}
	}
}

func TestIsDatacenterISP(t *testing.T) {
	datacenter := []string{
		"Amazon.com, Inc.",
		"Google LLC",
		"Microsoft Corporation",
		"DigitalOcean, LLC",
		"OVH SAS",
		"Hetzner Online GmbH",
		"Cloudflare, Inc.",
	}
	for _, isp := range datacenter {
		if !IsDatacenterISP(isp) {
			t.Errorf("%q 应该被识别为机房", isp)
		}
	}

	residential := []string{
		"Verizon Business",
		"Comcast Cable Communications",
		"Charter Communications",
		"Cox Communications",
		"",
		"Some Unknown ISP",
	}
	for _, isp := range residential {
		if IsDatacenterISP(isp) {
			t.Errorf("%q 不该被识别为机房", isp)
		}
	}
}

// ---------------------------------------------------------------- 端到端（可选）

// TestEvaluatePurityLive 对真实 IP 评估（需要网络）。
func TestEvaluatePurityLive(t *testing.T) {
	if os.Getenv("REG_FACTORY_TEST_LIVE_EGRESS") != "1" {
		t.Skip("live egress probes require explicit opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 8.8.8.8 是 Google DNS，一定是机房
	s := EvaluatePurity(ctx, &EgressInfo{IP: "8.8.8.8"})
	if s == nil {
		t.Fatal("不该返回 nil")
	}
	t.Logf("8.8.8.8 评估: %s", s.Note())
	if !s.Datacenter {
		t.Error("8.8.8.8 应该被判为机房")
	}
	if s.Total > 40 {
		t.Errorf("机房 IP 分数不该高于 40, 实际 %d", s.Total)
	}
}

func TestUnavailablePurityNeverEntersPool(t *testing.T) {
	s := evaluatePurity(context.Background(), &EgressInfo{IP: "192.0.2.1"},
		func(context.Context, string) (*ipAPIResult, error) { return nil, errors.New("unavailable") }, noBlacklists)
	if s.Pass("B") || s.Verified || s.Grade != "F" {
		t.Fatalf("unverified exit admitted: %+v", s)
	}
}
