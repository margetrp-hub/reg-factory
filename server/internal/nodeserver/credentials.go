package nodeserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
)

// credDir 是旧版节点本地凭据目录：/<email 前缀>.json。
// 手工生产的 Team JSON 现在落在 plus-import/team-json；两处都读，优先
// 新目录，避免 401 修复后仍把旧文件当成唯一事实来源。
const credDir = "/data/creds"

func dataDir() string {
	if value := strings.TrimSpace(os.Getenv("REG_FACTORY_DATA_DIR")); value != "" {
		return value
	}
	return "/data"
}

func credentialDirs() []string {
	root := dataDir()
	dirs := []string{filepath.Join(root, "plus-import", "team-json"), filepath.Join(root, "creds")}
	// Keep the historical absolute location available when REG_FACTORY_DATA_DIR
	// points elsewhere (some old images mounted /data/creds directly).
	if credDir != dirs[1] {
		dirs = append(dirs, credDir)
	}
	return dirs
}

func credFile(email string) string {
	local := strings.SplitN(email, "@", 2)[0]
	return filepath.Join(credDir, local+".json")
}

func emailFromDocument(doc map[string]any) string {
	for _, key := range []string{"email", "account_email", "user_email"} {
		if value, _ := doc[key].(string); strings.Contains(value, "@") {
			return strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"account", "user", "profile", "payload", "credentials", "document"} {
		if nested, ok := doc[key].(map[string]any); ok {
			if value := emailFromDocument(nested); value != "" {
				return value
			}
		}
	}
	return ""
}

func readJSON(path string) (map[string]any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || len(doc) == 0 {
		return nil, false
	}
	return doc, true
}

func slug(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func findCredential(email string) (map[string]any, string) {
	want := strings.ToLower(strings.TrimSpace(email))
	if want == "" {
		return nil, ""
	}
	// The producer names files team-<slug>.json.  Try the deterministic names
	// first, then scan the directory so older/manual names remain readable.
	for _, dir := range credentialDirs() {
		base := slug(want)
		for _, name := range []string{"team-" + base + ".json", base + ".json"} {
			if doc, ok := readJSON(filepath.Join(dir, name)); ok {
				found := emailFromDocument(doc)
				if found == "" || strings.EqualFold(found, want) {
					if found == "" {
						doc["email"] = email
					}
					return doc, filepath.Join(dir, name)
				}
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				continue
			}
			doc, ok := readJSON(filepath.Join(dir, entry.Name()))
			if ok && strings.EqualFold(emailFromDocument(doc), want) {
				return doc, filepath.Join(dir, entry.Name())
			}
		}
	}
	return nil, ""
}

// resolveEmail 从 assignment 里定位账号。
//
// 优先用 parameters.email（Core 知道明文邮箱）；没有就遍历凭据目录，用
// SubjectID(email) 反查 —— 这样即使 Core 只给 UUID 也能对上。
func (h *Handler) resolveEmail(verified *Verified) string {
	if verified == nil {
		return ""
	}
	if v, ok := verified.Params["email"].(string); ok && strings.Contains(v, "@") {
		return v
	}
	// Core 把邮箱放在授权载荷里：节点自己决定怎么存账号，
	// 但需要知道这次要动哪一个。
	if g, ok := verified.Params["custody_grant"].(map[string]any); ok {
		if pl, ok := g["payload"].(map[string]any); ok {
			if v, ok := pl["email"].(string); ok && strings.Contains(v, "@") {
				return v
			}
		}
	}
	subject, _ := verified.Payload["subject_id"].(string)
	if subject == "" {
		return ""
	}
	// Work's subject UUID is derived from the node account ref (for example
	// import:<email> or creds:<slug>), not from the bare email.  Resolve the
	// ref through the node account index first so refresh/authorize can still
	// locate the JSON after the account has been seeded into SQLite.
	if ref, ok := h.subjectRef(subject); ok && h.st != nil {
		if account, err := h.st.GetAccount(ref); err == nil && strings.Contains(account.Identity, "@") {
			return strings.TrimSpace(account.Identity)
		}
	}
	for _, dir := range credentialDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			doc, ok := readJSON(filepath.Join(dir, e.Name()))
			if !ok {
				continue
			}
			email := emailFromDocument(doc)
			if email != "" && (SubjectID(email) == subject ||
				SubjectID("import:"+email) == subject ||
				SubjectID("creds:"+email) == subject ||
				SubjectID("creds:"+strings.TrimSuffix(e.Name(), ".json")) == subject) {
				return email
			}
		}
	}
	return ""
}

// credDocument 读出该账号当前凭据。读不到返回 nil，表示这次没有可交付的工件。
func (h *Handler) credDocument(verified *Verified) (map[string]any, string) {
	email := h.resolveEmail(verified)
	if email == "" {
		return nil, ""
	}
	// The refresh executor writes the canonical asset store. Read that exact
	// source before historical producer files, which may still hold old tokens.
	if h.st != nil {
		if ref, ok := h.subjectRef(verified.Subject); ok {
			if asset, err := assetstore.New(dataDir()).Get(ref, email); err == nil && asset.Payload != nil {
				doc := normalizeDocument(asset.Payload, asset.Email)
				if doc != nil && strings.EqualFold(emailFromDocument(doc), email) {
					return doc, email
				}
			}
			// A known reference must not silently fall back to an old export.
			return nil, email
		}
	}
	doc, path := findCredential(email)
	if doc == nil {
		// Preserve the old direct path as a final compatibility read when a
		// producer did not include an email field in its JSON.
		if legacy, ok := readJSON(credFile(email)); ok {
			doc, path = legacy, credFile(email)
		}
	}
	if doc == nil {
		return nil, email
	}
	// Team JSON is a complete handoff document and may use session/access
	// token fields instead of refresh_token.  Only the legacy OAuth path keeps
	// the old refresh-token requirement.
	if !strings.Contains(filepath.ToSlash(path), "/plus-import/team-json/") {
		if v, _ := doc["refresh_token"].(string); v == "" {
			return nil, email
		}
	}
	if emailFromDocument(doc) == "" {
		doc["email"] = email
	}
	return doc, email
}
