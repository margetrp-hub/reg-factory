package mail

// 库分组
// ======
//
// 一个部署有多类邮箱，邮件正文不能全塞一个库：
//   240 个 Outlook × 每号几十封 → 万级
//   单库超过几 GB 后 SQLite 写入锁和全表扫都会拖垮
//
// 所以拆的是「邮件正文」，不是「池」：
//
//   index.db     mail_pool / mail_platform_bans / mail_sources
//                元数据，量小，集中管理，是取号的唯一入口
//
//   mail/*.db    mail_messages
//                按 group 拆，每个 group 一个文件
//
// 取号永远查 index.db，拿到 email 后再去对应 group 库读邮件。
//
// GroupOf 决定一个邮箱的邮件落在哪个库。默认按 provider 分，
// 但可以在 mail_pool.db_group 上手工覆盖（比如把一批 Outlook
// 拆成 outlook-hot / outlook-cold 做冷热分离）。

import (
	"path/filepath"
	"strings"
)

// 默认分组名
const (
	GroupOutlook   = "outlook"
	GroupCloudmail = "cloudmail"
	GroupQQ        = "qq"
	GroupICloud    = "icloud"
	GroupGmail     = "gmail"
	GroupNetease   = "netease"
	GroupMisc      = "misc"
)

// GroupOf 按 provider 推出默认的库分组。
//
// provider 是邮箱服务商标识（outlook / cloudmail / qq / icloud / ...），
// 一个 provider 默认一个库。要更细的拆分就在 db_group 上覆盖。
func GroupOf(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "outlook", "hotmail", "live", "msn":
		return GroupOutlook
	case "cloudmail", "skymail":
		return GroupCloudmail
	case "qq", "foxmail":
		return GroupQQ
	case "icloud", "me", "mac":
		return GroupICloud
	case "gmail", "googlemail":
		return GroupGmail
	case "163", "126", "yeah", "netease":
		return GroupNetease
	}
	return GroupMisc
}

// GroupDBPath 返回某个 group 的邮件库路径。
//
// root 通常是 /data/mail。
//
//	/data/mail/pools/outlook.db
func GroupDBPath(root, group string) string {
	g := strings.TrimSpace(group)
	if g == "" {
		g = GroupMisc
	}
	// 只留安全字符，防止路径穿越
	var b strings.Builder
	for _, r := range g {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	name := b.String()
	if name == "" {
		name = GroupMisc
	}
	return filepath.Join(root, "mail", name+".db")
}

// IndexDBPath 返回索引库路径。
func IndexDBPath(root string) string {
	return filepath.Join(root, "index.db")
}

// ---------------------------------------------------------------- 取号过滤

// TakeFilter 是取号时的筛选条件。
//
// 所有字段为空表示「不限」，从全部可用里挑。
// 任务可以只给 Platform（排除该平台已封的），
// 也可以额外指定 Provider / Group / Role 做更细的筛选。
type TakeFilter struct {
	// Platform 必填：要排除在这个平台上已被封的邮箱
	Platform string

	// Provider 可选：只要某个服务商（outlook / cloudmail / qq ...）
	Provider string
	// Group 可选：只要某个库分组
	Group string
	// Role 可选：只要某个角色（address / alias / hub）
	Role string

	// ExcludeRoles 排除某些角色（比如取号时不要 hub）
	ExcludeRoles []string

	// RequireParent 为 true 时只取有母号的（IC 地址）
	RequireParent bool
}

// IsEmpty 判断是否只做了最低限度的过滤。
func (f TakeFilter) IsEmpty() bool {
	return f.Provider == "" && f.Group == "" && f.Role == "" &&
		len(f.ExcludeRoles) == 0 && !f.RequireParent
}

// Describe 返回人类可读的筛选说明，给日志用。
func (f TakeFilter) Describe() string {
	var parts []string
	if f.Platform != "" {
		parts = append(parts, "platform="+f.Platform)
	}
	if f.Provider != "" {
		parts = append(parts, "provider="+f.Provider)
	}
	if f.Group != "" {
		parts = append(parts, "group="+f.Group)
	}
	if f.Role != "" {
		parts = append(parts, "role="+f.Role)
	}
	if len(f.ExcludeRoles) > 0 {
		parts = append(parts, "not_role="+strings.Join(f.ExcludeRoles, ","))
	}
	if f.RequireParent {
		parts = append(parts, "has_parent")
	}
	if len(parts) == 0 {
		return "任意可用"
	}
	return strings.Join(parts, " ")
}
