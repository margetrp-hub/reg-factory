package mail

// 邮箱在池子里扮演的角色
// ===================
//
// hub      收件总线。唯一真正连 IMAP 的那一个，所有转发邮件都落在这里。
//          一个部署通常只需要一个 hub。
//
// address  普通地址。自己连 IMAP（QQ/Gmail/163），或者自己走 API（cloudmail/luckmail）。
//
// alias    别名。靠 hub 转发，自己不连 IMAP。
//          IC 隐藏邮箱就是这一类：Apple 把信转发到 hub，
//          hub 收下来后按 alias_hme 路由回具体地址。
//
// parent   母号。IC 的父 Apple ID。本身可能不参与注册，
//          只用来建立级联关系：母号被封 → 名下所有 alias 一起废。

const (
	RoleHub     = "hub"
	RoleAddress = "address"
	RoleAlias   = "alias"
	RoleParent  = "parent"
)

// 取信方式
// ======
//
// imap      自己连 IMAP（Host:Port + 凭证）
// pop3      自己连 POP3
// graph     Microsoft Graph API（refresh_token 换 access_token）
// outlook   Outlook REST
// cloudmail 自建邮件服务（Endpoint + JWT）
// luckmail  商业临时邮箱 API（Endpoint + APIKey）
// inbucket  自建 SMTP 测试服务（Endpoint）
// forward   不自己收，靠 hub 转发
// none      完全不需要收信
const (
	TransportIMAP      = "imap"
	TransportPOP3      = "pop3"
	TransportGraph     = "graph"
	TransportOutlook   = "outlook"
	TransportCloudmail = "cloudmail"
	TransportLuckmail  = "luckmail"
	TransportInbucket  = "inbucket"
	TransportForward   = "forward"
	TransportNone      = "none"
)

// TransportNeedsSource 判断一个 transport 是否需要自己的 mail_sources 记录。
//
// forward / none 不需要 —— 它们不出现在 Runner 的轮询列表里。
func TransportNeedsSource(transport string) bool {
	switch transport {
	case TransportForward, TransportNone, "":
		return false
	}
	return true
}

// RoleIsPassive 判断一个角色是否只被动收信（不需要自己的源）。
func RoleIsPassive(role string) bool {
	return role == RoleAlias || role == RoleParent
}

// NormalizeRole 把外部输入归一到合法的 role。
func NormalizeRole(v string) string {
	switch v {
	case RoleHub, RoleAddress, RoleAlias, RoleParent:
		return v
	case "":
		return RoleAddress
	}
	return RoleAddress
}

// NormalizeTransport 把外部输入归一到合法的 transport。
func NormalizeTransport(v string) string {
	switch v {
	case TransportIMAP, TransportPOP3, TransportGraph, TransportOutlook,
		TransportCloudmail, TransportLuckmail, TransportInbucket,
		TransportForward, TransportNone:
		return v
	}
	return TransportIMAP
}

// PreferredTransports 返回某个 provider 的取信顺序，照搬
// microsoft_provider_sequence 的三级回退思路。
//
// Outlook 的三条路：先 IMAP（最简单），失败退 Graph，再退 REST。
func PreferredTransports(provider string) []string {
	switch provider {
	case "outlook", "hotmail", "live":
		return []string{TransportIMAP, TransportGraph, TransportOutlook}
	case "icloud", "me", "mac":
		return []string{TransportIMAP}
	case "cloudmail", "skymail":
		return []string{TransportCloudmail}
	case "luckmail":
		return []string{TransportLuckmail}
	case "inbucket":
		return []string{TransportInbucket}
	}
	return []string{TransportIMAP, TransportPOP3}
}
