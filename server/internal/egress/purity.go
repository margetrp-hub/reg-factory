package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// IP 纯净度检测
// -----------
// 「能过 Cloudflare」只说明这个 IP 没被 CF 拉黑，完全不代表它干净：
//   · 数据中心 IP 也能过 CF，但 Radar 一看 ASN 就知道是机器
//   · 被滥用的住宅 IP 也能过 CF，但 Fraud Score 可能 90+
//   · 代理/VPN 出口也能过 CF，但 ip-api 的 proxy 字段是 true
//
// 所以纯净度要分多个维度打分，只有综合分够高的才入库。
//
// 数据源（都免费，不需要 key）
//   1. ip-api.com       proxy / hosting / mobile / asn 标记，45 req/min
//   2. ipinfo.io        基础归属信息
//   3. DNSBL 查询       Spamhaus ZEN 等主要黑名单
//
// 评分维度与权重
//   机房 IP          直接淘汰（权重最高）
//   proxy 标记       -40
//   VPN 标记         -40
//   Tor 出口         直接淘汰
//   在 3 个以上黑名单 直接淘汰
//   在 1-2 个黑名单   -25
//   延迟 > 3 秒       -15
//   延迟 > 1.5 秒     -8
//   住宅型 ASN        +20
//   移动网络          +10（但有些平台不认移动 IP）
//   地理位置与账号匹配 +15
//
// 分级
//   A  85-100   优秀，优先取用
//   B  70-84    良好，可用
//   C  50-69    勉强，降权使用
//   D  30-49    差，不推荐
//   F  0-29     淘汰

// PurityScore 是一个 IP 的纯净度评估。
type PurityScore struct {
	Verified bool `json:"verified"`
	// 原始信号
	Datacenter bool     `json:"datacenter"`
	Proxy      bool     `json:"proxy"`
	VPN        bool     `json:"vpn"`
	Tor        bool     `json:"tor"`
	Mobile     bool     `json:"mobile"`
	Hosting    bool     `json:"hosting"`
	Blacklists []string `json:"blacklists"`
	AbuseCount int      `json:"abuse_count"`

	// 综合
	Total   int      `json:"total"`   // 0-100
	Grade   string   `json:"grade"`   // A/B/C/D/F
	Reasons []string `json:"reasons"` // 扣分/加分理由
}

// Pass 判断是否达到入库标准（默认 B 级以上）。
func (p *PurityScore) Pass(minGrade string) bool {
	return p != nil && p.Verified && !p.Datacenter && !p.Hosting && gradeRank(p.Grade) >= gradeRank(minGrade)
}

func gradeRank(g string) int {
	switch strings.ToUpper(g) {
	case "A":
		return 4
	case "B":
		return 3
	case "C":
		return 2
	case "D":
		return 1
	}
	return 0
}

// gradeOf 把分数映射成等级。
func gradeOf(score int) string {
	switch {
	case score >= 85:
		return "A"
	case score >= 70:
		return "B"
	case score >= 50:
		return "C"
	case score >= 30:
		return "D"
	}
	return "F"
}

// ---------------------------------------------------------------- ip-api 探测

// ipAPIResult 是 ip-api.com 的响应。
type ipAPIResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Country string `json:"countryCode"`
	Region  string `json:"regionName"`
	City    string `json:"city"`
	ISP     string `json:"isp"`
	Org     string `json:"org"`
	AS      string `json:"as"`
	Mobile  bool   `json:"mobile"`
	Proxy   bool   `json:"proxy"`
	Hosting bool   `json:"hosting"`
	Query   string `json:"query"`
}

// probeIPAPI 查 ip-api.com。
func probeIPAPI(ctx context.Context, ip string) (*ipAPIResult, error) {
	if ip == "" {
		return nil, fmt.Errorf("ip 为空")
	}
	u := fmt.Sprintf(
		"http://ip-api.com/json/%s?fields=status,message,countryCode,regionName,city,isp,org,as,mobile,proxy,hosting,query",
		ip)

	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ip-api HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))

	var out ipAPIResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Status != "success" || out.Query != ip {
		return nil, fmt.Errorf("ip-api: %s", out.Message)
	}
	return &out, nil
}

// ---------------------------------------------------------------- DNSBL 查询

// dnsblZones 是几个主要的 IP 黑名单。
//
// 查询方式：把 IP 反序拼上 zone，做一次 DNS A 记录查询。
// 有返回就说明在名单里。
var dnsblZones = []struct {
	name string
	zone string
}{
	{"Spamhaus ZEN", "zen.spamhaus.org"},
	{"SpamCop", "bl.spamcop.net"},
	{"SORBS", "dnsbl.sorbs.net"},
	{"Barracuda", "b.barracudacentral.org"},
}

// checkDNSBL 查 IP 在多少黑名单里。
//
// 用 net.Resolver 的 LookupHost，有 A 记录返回即在名单中。
func checkDNSBL(ctx context.Context, ip string) []string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return nil
	}
	reversed := fmt.Sprintf("%s.%s.%s.%s", parts[3], parts[2], parts[1], parts[0])

	var hits []string
	for _, z := range dnsblZones {
		host := reversed + "." + z.zone
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err := net.DefaultResolver.LookupHost(cctx, host)
		cancel()
		if err == nil {
			for _, addr := range addrs {
				// Ignore resolver wildcards and DNSBL service/error answers.
				parsed := net.ParseIP(addr).To4()
				if parsed != nil && parsed[0] == 127 && parsed[1] == 0 && parsed[2] == 0 && parsed[3] > 0 {
					hits = append(hits, z.name)
					break
				}
			}
		}
	}
	return hits
}

// ---------------------------------------------------------------- 主评估

// EvaluatePurity 评估一个 IP 的纯净度。
//
// 需要先有出口信息（来自 ProbeEgress），这里只做加测。
func EvaluatePurity(ctx context.Context, info *EgressInfo) *PurityScore {
	return evaluatePurity(ctx, info, probeIPAPI, checkDNSBL)
}

func evaluatePurity(ctx context.Context, info *EgressInfo, probe func(context.Context, string) (*ipAPIResult, error), blacklist func(context.Context, string) []string) *PurityScore {
	s := &PurityScore{}

	if info == nil || info.IP == "" {
		s.Grade = "F"
		s.Reasons = append(s.Reasons, "无出口信息")
		return s
	}

	// ipinfo 的 org 已经给了 ISP，先用它粗判
	if IsDatacenterISP(info.ISP) {
		s.Datacenter = true
		s.Hosting = true
		s.Reasons = append(s.Reasons, "ISP 显示为机房: "+info.ISP)
	}

	// 步骤 1：ip-api 补充标记
	api, err := probe(ctx, info.IP)
	if err == nil && api != nil {
		s.Verified = true
		if api.Proxy {
			s.Proxy = true
			s.Reasons = append(s.Reasons, "ip-api 标记为代理")
		}
		if api.Hosting {
			s.Hosting = true
			s.Datacenter = true
			s.Reasons = append(s.Reasons, "ip-api 标记为托管")
		}
		if api.Mobile {
			s.Mobile = true
			s.Reasons = append(s.Reasons, "移动网络")
		}
		// ASN 里带 hosting 关键字也算
		if strings.Contains(strings.ToLower(api.AS), "hosting") {
			s.Hosting = true
			s.Reasons = append(s.Reasons, "ASN 含 hosting: "+api.AS)
		}
	} else if err != nil {
		s.Reasons = append(s.Reasons, "ip-api 查询失败: "+Truncate(err.Error(), 40))
	}

	// 步骤 2：黑名单
	hits := blacklist(ctx, info.IP)
	s.Blacklists = hits
	if len(hits) > 0 {
		s.Reasons = append(s.Reasons, "黑名单: "+strings.Join(hits, ", "))
	}

	// 步骤 3：打分
	if !s.Verified || ctx.Err() != nil {
		s.Verified = false
		s.Grade = "F"
		s.Reasons = append(s.Reasons, "出口评估未完成，不进入可用池")
		return s
	}
	score := 100

	// 硬性淘汰
	if s.Tor {
		return &PurityScore{Grade: "F", Total: 0,
			Reasons: append(s.Reasons, "Tor 出口")}
	}
	if len(hits) >= 3 {
		s.Total = 0
		s.Grade = "F"
		s.Reasons = append(s.Reasons, "黑名单命中 ≥3")
		return s
	}

	// 扣分项
	if s.Datacenter {
		score -= 60
	}
	if s.Proxy {
		score -= 40
	}
	if s.VPN {
		score -= 40
	}
	if len(hits) == 1 {
		score -= 25
	} else if len(hits) == 2 {
		score -= 35
	}
	switch {
	case info.LatencyMs > 3000:
		score -= 15
		s.Reasons = append(s.Reasons, fmt.Sprintf("延迟 %dms", info.LatencyMs))
	case info.LatencyMs > 1500:
		score -= 8
		s.Reasons = append(s.Reasons, fmt.Sprintf("延迟 %dms", info.LatencyMs))
	}

	// 加分项
	if !s.Datacenter && !s.Hosting && !s.Proxy {
		score += 20
		s.Reasons = append(s.Reasons, "住宅型出口")
	}
	if s.Mobile {
		score += 5
	}

	// 边界
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	s.Total = score
	s.Grade = gradeOf(score)
	return s
}

// purityNote 生成给日志看的一行摘要。
func (s *PurityScore) Note() string {
	if s == nil {
		return "(未评估)"
	}
	parts := []string{fmt.Sprintf("%s级 %d分", s.Grade, s.Total)}
	if s.Datacenter {
		parts = append(parts, "机房")
	}
	if s.Proxy {
		parts = append(parts, "代理")
	}
	if len(s.Blacklists) > 0 {
		parts = append(parts, fmt.Sprintf("黑名单%d", len(s.Blacklists)))
	}
	return strings.Join(parts, " ")
}
