// Package classify turns a received message into stable tags so the mailbox
// layer can group mail and drive per-alias state without any LLM call.
//
// Rules are intentionally weighted, not scored per keyword: `banned` must win
// whenever it fires, because a false positive is cheap to review while a false
// negative costs an account. Only the subject plus the first 2000 characters of
// the body are scanned — notification mail states its verdict up front, and
// scanning quoted boilerplate in the tail creates false hits.
package icmail

import (
	"regexp"
	"strings"
)

// Tag names shared with the UI and the database.
const (
	TagBanned  = "banned"
	TagRisk    = "risk"
	TagOTP     = "otp"
	TagPayment = "payment"
	TagPromo   = "promo"
	TagOther   = "other"
)

const scanLimit = 2000

// Weights decide the primary tag when several rules fire.
var weights = map[string]int{
	TagBanned:  100,
	TagRisk:    60,
	TagOTP:     40,
	TagPayment: 20,
	TagPromo:   10,
}

var tagOrder = []string{TagBanned, TagRisk, TagOTP, TagPayment, TagPromo}

type rule struct {
	tag      string
	name     string
	re       *regexp.Regexp
	cjkInput bool // run against the raw text instead of the lower-cased copy
}

func mustRule(tag, name, pattern string, cjk bool) rule {
	return rule{tag: tag, name: name, re: regexp.MustCompile(pattern), cjkInput: cjk}
}

var rules = []rule{
	// ---- banned / terminated ---------------------------------------------
	mustRule(TagBanned, "zh_ban",
		`封禁|封号|封號|被封|停權|停权|永久停用|已被停用|帳號被停用|账号被停用|永久凍結|永久冻结|冻结账号|凍結帳號|禁止使用|終止服務|终止服务|封停|停封|已封号`, true),
	// "冻结" alone is not enough (frozen food promos), but "account … frozen"
	// and "violation … frozen/blocked" are unambiguous enforcement wording.
	mustRule(TagBanned, "zh_frozen_account",
		`(账号|帳號|帐号|账户|帳戶|會員|会员)[^。\n]{0,20}(冻结|凍結|封停|停封|封禁|被限制)|`+
			`(违规|違規|違反)[^。\n]{0,40}(冻结|凍結|封禁|封號|封号|停用|限制|禁止|终止|終止)`, true),
	mustRule(TagBanned, "zh_violation",
		`違反.{0,12}(規範|條款|政策|準則)|违反.{0,12}(规定|規定|条款|條款|政策|准则)|帳號.{0,8}(異常|异常).{0,12}(終止|終了|停用|限制)|账号.{0,8}(异常|異常).{0,12}(终止|停用|限制)`, true),
	mustRule(TagBanned, "en_ban",
		`\baccount\b[^.]{0,40}\b(has been|was|is|will be)\b[^.]{0,20}\b(banned|suspended|disabled|terminated|deactivated|closed|restricted)\b|`+
			`\bpermanently (banned|suspended|disabled|terminated)\b|`+
			`\b(we|i)('ve| have) (banned|suspended|disabled|terminated)\b|`+
			`\baccount (ban|suspension|termination|closure)\b`, false),
	mustRule(TagBanned, "en_policy",
		`\bviolat\w*\b[^.]{0,30}\b(terms|policy|policies|guidelines|agreement)\b|`+
			`\bbreach\w*\b[^.]{0,20}\b(terms|policy|policies)\b|`+
			`\b(terms of service|community guidelines)\b[^.]{0,20}\bviolat`, false),
	mustRule(TagBanned, "ja_ban", `アカウント[^。]{0,20}(停止|無効|利用停止|抹消)|利用規約[^。]{0,10}違反`, true),

	// ---- risk / verification ---------------------------------------------
	mustRule(TagRisk, "zh_risk",
		`風險控制|风险控制|風控|异常活动|異常活動|異常登入|异常登录|異地登入|异地登录|安全提醒|帳號安全|账号安全|需要驗證|需要验证|請重新登入|请重新登录|身份驗證|身份验证|人機驗證|人机验证|帳號受限|账号受限`, true),
	mustRule(TagRisk, "en_risk",
		`\b(suspicious|unusual|unrecognized|unexpected)\b[^.]{0,30}\b(activity|login|sign-?in|device|access)\b|`+
			`\bsecurity alert\b|\bverify your (identity|account|email)\b|\bconfirm your (identity|account)\b|`+
			`\baccount (flagged|locked)\b|\b(two|2)-?step verification\b|\bsign-?in attempt\b`, false),

	// ---- otp / code -------------------------------------------------------
	// Never match a bare number here: order ids, prices and dates look exactly
	// like a 6 digit code and would bury the real signal.
	mustRule(TagOTP, "code",
		`驗證碼|验证码|verification code|security code|login code|sign-?in code|`+
			`one[- ]?time (password|code|passcode)|\bOTP\b|\b2FA\b|兩步驗證|两步验证|`+
			`\b\d{4,8}\b[^.\n]{0,24}\b(is your|code|驗證碼|验证码)\b|`+
			`(code|驗證碼|验证码)[^.\n]{0,24}\b\d{4,8}\b`, true),

	// ---- money ------------------------------------------------------------
	mustRule(TagPayment, "money",
		`收據|收据|發票|发票|對帳單|对账单|付款|付費|付费|訂單|订单|訂閱|订阅|`+
			`\breceipt\b|\binvoice\b|\bpayment\b|\bcharged\b|\brefund\b|\bsubscription\b|\border (confirm|receipt|number)\b`, true),

	// ---- bulk / marketing -------------------------------------------------
	mustRule(TagPromo, "promo",
		`促銷|促销|優惠|优惠|折扣|限時|限时|免費領取|免费领取|`+
			`\bunsubscribe\b|\bnewsletter\b|\bpromotion\b|\bdiscount\b|\bsale\b`, true),
}

// Verdict is the classifier output for one message.
type Verdict struct {
	Tags       []string `json:"tags"`
	PrimaryTag string   `json:"primaryTag"`
	Score      int      `json:"score"`
	Matches    []string `json:"matches"`
	Banned     bool     `json:"banned"`
}

// Classify inspects one message and returns its tags.
func Classify(subject, sender, body string) Verdict {
	if len(body) > scanLimit {
		body = body[:scanLimit]
	}
	text := subject + "\n" + sender + "\n" + body
	lowered := strings.ToLower(text)

	scores := map[string]int{}
	var matches []string
	for _, r := range rules {
		haystack := lowered
		if r.cjkInput {
			haystack = text
		}
		if r.re.MatchString(haystack) {
			matches = append(matches, r.name)
			scores[r.tag] += weights[r.tag]
		}
	}

	var tags []string
	for _, tag := range tagOrder {
		if _, ok := scores[tag]; ok {
			tags = append(tags, tag)
		}
	}
	primary := TagOther
	score := 0
	if len(tags) > 0 {
		primary = tags[0]
		score = scores[primary]
	}
	if matches == nil {
		matches = []string{}
	}
	if tags == nil {
		tags = []string{}
	}
	return Verdict{
		Tags:       tags,
		PrimaryTag: primary,
		Score:      score,
		Matches:    matches,
		Banned:     scores[TagBanned] > 0,
	}
}

// StateForTags maps a tag set to an alias state, or "" when nothing changed.
func StateForTags(tags []string) string {
	for _, tag := range tags {
		if tag == TagBanned {
			return TagBanned
		}
	}
	for _, tag := range tags {
		if tag == TagRisk {
			return TagRisk
		}
	}
	return ""
}
