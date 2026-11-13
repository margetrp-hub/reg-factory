// Package assetstore 是账号资产的唯一汇聚地与读写入口。
//
// 目录约定（所有平台资产都必须落在这里，不再散落到 creds/ cookies/ tokens/）：
//
//	/data/assets/<platform>/<kind>/<ref>.json
//
// 每个文件是一个 JSON 对象，公共字段：
//
//	{"ref":"creds:xxx","platform":"chatgpt","kind":"oauth","email":"a@b.com",
//	 "created_at":"...","source":"migrated","payload":{...平台原始字段...}}
//
// 旧路径（creds/、cookies/、tokens/）由 Migrate 一次性搬进来，读取时保持兼容。
package assetstore

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kinds 是允许的资产类型。
var Kinds = map[string]bool{
	"oauth":    true, // ChatGPT/Codex OAuth 凭据
	"session":  true, // 网页会话（sessionKey / cookie）
	"sso":      true, // Grok SSO
	"password": true, // 邮箱+密码（Outlook/iCloud）
	"imported": true, // 人工导入
	"free":     true, // 已转成 Free 凭据（access_token）
	"team":     true,
}

// Asset 是一条资产记录。
type Asset struct {
	Ref                 string         `json:"ref"`
	Platform            string         `json:"platform"`
	Kind                string         `json:"kind"`
	Email               string         `json:"email"`
	Source              string         `json:"source"`
	CreatedAt           string         `json:"created_at"`
	CredentialUpdatedAt string         `json:"credential_updated_at,omitempty"`
	CredentialRef       string         `json:"credential_ref,omitempty"`
	Payload             map[string]any `json:"payload,omitempty"`
	// Path 是文件相对 assets 根的路径（不进 JSON）
	Path      string `json:"-"`
	ResultSHA string `json:"result_sha256,omitempty"`
	Current   bool   `json:"is_current,omitempty"`
}

// Store 是资产库。
type Store struct {
	root string
}

// New 创建资产库（root 通常是 <dataDir>/assets）。
func New(dataDir string) *Store {
	return &Store{root: filepath.Join(dataDir, "assets")}
}

// Root 返回资产根目录。
func (s *Store) Root() string { return s.root }

func (s *Store) dir(platform, kind string) string {
	return filepath.Join(s.root, safe(platform), safe(kind))
}

func safe(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '@':
			return r
		default:
			return '_'
		}
	}, value)
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}

// documentEmail accepts both the flat Team JSON shape and the nested
// “payload/credentials/tokens“ envelopes used by older SG producers.
// Migrations must retain the account identity even when the producer did not
// duplicate it at the top level.
func documentEmail(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"email", "account_email", "user_email", "username", "login"} {
		if candidate, ok := object[key].(string); ok && strings.Contains(candidate, "@") {
			return strings.TrimSpace(candidate)
		}
	}
	for _, key := range []string{"payload", "credentials", "oauth", "token", "tokens", "account", "user", "profile", "document"} {
		if email := documentEmail(object[key]); email != "" {
			return email
		}
	}
	return ""
}

// Put atomically publishes the current projection. Refresh recovery explicitly
// pins its input with Snapshot; ordinary imports do not retain old credentials.
func (s *Store) Put(asset Asset) error {
	if s.withdrawn(asset) {
		return fmt.Errorf("ASSET_SOURCE_TRANSFERRED")
	}
	if asset.Ref == "" || asset.Platform == "" || asset.Kind == "" {
		return fmt.Errorf("asset 缺少 ref/platform/kind")
	}
	if !Kinds[asset.Kind] {
		return fmt.Errorf("未知资产类型: %s", asset.Kind)
	}
	unlock, err := s.lock(asset)
	if err != nil {
		return err
	}
	defer unlock()
	if asset.CreatedAt == "" {
		asset.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if asset.Platform == "chatgpt" && !strings.HasPrefix(asset.Source, "migrated:") {
		if doc, err := OAuthDocument(asset.Payload, asset.Email); err == nil && credentialString(doc, "refresh_token") != "" {
			asset.CredentialUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	dir := s.dir(asset.Platform, asset.Kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := safe(strings.ReplaceAll(strings.TrimPrefix(asset.Ref, "creds:"), "/", "_"))
	path := filepath.Join(dir, name+".json")

	asset.ResultSHA = ""
	raw, err := json.MarshalIndent(asset, "", "  ")
	if err != nil {
		return err
	}
	if asset.Platform != "chatgpt" {
		if old, err := os.ReadFile(path); err == nil {
			if _, err = s.Retain(old); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err = s.Retain(raw); err != nil {
			return err
		}
	}
	return atomicWrite(path, raw)
}

// PutIfAbsent 只在目标不存在时写入（增量迁移用，避免反复覆盖时间戳）。
func (s *Store) PutIfAbsent(asset Asset) (bool, error) {
	dir := s.dir(asset.Platform, asset.Kind)
	name := safe(strings.ReplaceAll(strings.TrimPrefix(asset.Ref, "creds:"), "/", "_"))
	path := filepath.Join(dir, name+".json")
	if _, err := os.Stat(path); err == nil {
		if asset.Platform == "chatgpt" {
			return false, nil
		}
		// Preserve the observed legacy result without selecting it as current.
		raw, marshalErr := json.MarshalIndent(asset, "", "  ")
		if marshalErr != nil {
			return false, marshalErr
		}
		_, retainErr := s.Retain(raw)
		if retainErr != nil {
			return false, retainErr
		}
		return false, nil
	}
	if err := s.Put(asset); err != nil {
		return false, err
	}
	return true, nil
}

// List 遍历资产根，返回全部资产。
func (s *Store) List() ([]Asset, error) {
	out := make([]Asset, 0, 64)
	if _, err := os.Stat(s.root); err != nil {
		return out, nil
	}
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		var asset Asset
		if jsonErr := json.Unmarshal(raw, &asset); jsonErr != nil {
			return nil
		}
		if asset.Ref == "" || s.withdrawn(asset) {
			return nil
		}
		if asset.Platform == "chatgpt" {
			var envelope map[string]any
			if json.Unmarshal(raw, &envelope) == nil {
				if current, err := OAuthDocument(envelope, asset.Email); err == nil {
					asset.Payload = current
				}
			}
		}
		asset.ResultSHA = fmt.Sprintf("%x", sha256.Sum256(raw))
		if rel, relErr := filepath.Rel(s.root, path); relErr == nil {
			asset.Path = rel
		} else {
			asset.Path = path
		}
		if asset.CreatedAt == "" {
			if info, statErr := entry.Info(); statErr == nil {
				asset.CreatedAt = info.ModTime().Format(time.RFC3339)
			}
		}
		out = append(out, asset)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Ref < out[j].Ref
	})
	return out, err
}

// Get 按 ref 取资产。
func (s *Store) Get(ref string, knownEmail ...string) (Asset, error) {
	probe := Asset{Ref: ref, Platform: "chatgpt"}
	if len(knownEmail) == 1 {
		probe.Email = knownEmail[0]
	}
	if s.withdrawn(probe) {
		return Asset{}, fmt.Errorf("ASSET_SOURCE_TRANSFERRED")
	}
	list, err := s.List()
	if err != nil {
		return Asset{}, err
	}
	found := false
	for _, item := range list {
		if item.Ref == ref {
			found = true
			break
		}
	}
	if !found && len(knownEmail) == 1 && strings.Contains(knownEmail[0], "@") {
		list = append(list, Asset{Ref: ref, Platform: "chatgpt", Kind: "imported", Email: knownEmail[0]})
	}
	return s.current(ref, list)
}

// Migrate 把旧路径的资产搬进统一目录（幂等，不删除源文件）。
//
//	creds/*.json          -> chatgpt/oauth
//	cookies/<platform>/*  -> <platform>/session
//	tokens/<platform>/*   -> <platform>/oauth
func (s *Store) Migrate(dataDir string) (int, error) {
	moved := 0

	// 1) creds/*.json（ChatGPT OAuth）
	credsDir := filepath.Join(dataDir, "creds")
	if entries, err := os.ReadDir(credsDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				continue
			}
			raw, readErr := os.ReadFile(filepath.Join(credsDir, entry.Name()))
			if readErr != nil {
				continue
			}
			var payload map[string]any
			if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
				continue
			}
			email := documentEmail(payload)
			name := strings.TrimSuffix(entry.Name(), ".json")
			asset := Asset{
				Ref: "creds:" + name, Platform: "chatgpt", Kind: "oauth",
				Email: email, Source: "migrated:creds", Payload: payload,
			}
			if added, err := s.PutIfAbsent(asset); err == nil && added {
				moved++
			}
		}
	}

	// 1b) Legacy Team JSON is evidence, never authority to overwrite current
	// credentials. Explicit production/export selects a retained result digest.
	teamDir := filepath.Join(dataDir, "plus-import", "team-json")
	if entries, err := os.ReadDir(teamDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				continue
			}
			raw, readErr := os.ReadFile(filepath.Join(teamDir, entry.Name()))
			if readErr != nil {
				continue
			}
			var payload map[string]any
			if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
				continue
			}
			email := documentEmail(payload)
			if !strings.Contains(email, "@") {
				continue
			}
			// Imported accounts already use import:<email> as their stable
			// account ref.  Reuse that ref so a hand-generated Team JSON refreshes
			// the same row instead of creating a second offer for one mailbox.
			asset := Asset{
				Ref: "import:" + strings.ToLower(strings.TrimSpace(email)), Platform: "chatgpt", Kind: "oauth",
				Email: email, Source: "migrated:team-json", Payload: payload,
			}
			if added, err := s.PutIfAbsent(asset); err != nil {
				return moved, err
			} else if added {
				moved++
			}
		}
	}

	// 2) cookies/<platform>/*.json
	cookiesDir := filepath.Join(dataDir, "cookies")
	if platforms, err := os.ReadDir(cookiesDir); err == nil {
		for _, platform := range platforms {
			if !platform.IsDir() {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(cookiesDir, platform.Name()))
			for _, file := range files {
				if !strings.HasSuffix(strings.ToLower(file.Name()), ".json") {
					continue
				}
				raw, readErr := os.ReadFile(filepath.Join(cookiesDir, platform.Name(), file.Name()))
				if readErr != nil {
					continue
				}
				var payload map[string]any
				if json.Unmarshal(raw, &payload) != nil {
					continue
				}
				name := strings.TrimSuffix(file.Name(), ".json")
				email := documentEmail(payload)
				asset := Asset{
					Ref: platform.Name() + ":" + name, Platform: platform.Name(), Kind: "session",
					Email: email, Source: "migrated:cookies", Payload: payload,
				}
				if err := s.Put(asset); err == nil {
					moved++
				}
			}
		}
	}

	// 3) tokens/<platform>/*
	tokensDir := filepath.Join(dataDir, "tokens")
	if platforms, err := os.ReadDir(tokensDir); err == nil {
		for _, platform := range platforms {
			if !platform.IsDir() {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(tokensDir, platform.Name()))
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(strings.ToLower(file.Name()), ".json") {
					continue
				}
				raw, readErr := os.ReadFile(filepath.Join(tokensDir, platform.Name(), file.Name()))
				if readErr != nil {
					continue
				}
				var payload map[string]any
				if json.Unmarshal(raw, &payload) != nil {
					continue
				}
				name := strings.TrimSuffix(file.Name(), ".json")
				email := documentEmail(payload)
				asset := Asset{
					Ref: "tokens:" + platform.Name() + ":" + name, Platform: platform.Name(), Kind: "oauth",
					Email: email, Source: "migrated:tokens", Payload: payload,
				}
				if platform.Name() == "chatgpt" {
					if added, err := s.PutIfAbsent(asset); err != nil {
						return moved, err
					} else if added {
						moved++
					}
					continue
				}
				if err := s.Put(asset); err == nil {
					moved++
				}
			}
		}
	}

	return moved, nil
}

// Stats 按平台+类型汇总。
func (s *Store) Stats() (map[string]map[string]int, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int{}
	for _, asset := range list {
		if out[asset.Platform] == nil {
			out[asset.Platform] = map[string]int{}
		}
		out[asset.Platform][asset.Kind]++
	}
	return out, nil
}
