package nodeserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// accountsInventory 是账号自报端点：节点声明「我这里有谁」，Work 聚合成
// account_id -> node_id 索引后派单。
//
// 归属不再由任何一方登记 —— 节点是唯一知道自己持有什么的一方，声明即事实。
// 换节点、扩容、退役都会在下次自报时自动收敛，不需要修改任何绑定表。
//
// 只返回摘要，不返回凭据本体；凭据只在交付工件时出现。
func (h *Handler) accountsInventory(c *gin.Context) {
	limit := 200
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		if v > 500 {
			v = 500
		}
		limit = v
	}
	since := int64(0)
	if v, err := strconv.ParseInt(c.Query("since"), 10, 64); err == nil {
		since = v
	}
	after, _ := base64.URLEncoding.DecodeString(c.Query("cursor"))

	dirs := append(credentialDirs(),
		filepath.Join(dataDir(), "assets", "chatgpt", "oauth"),
		filepath.Join(dataDir(), "assets", "chatgpt", "imported"),
	)
	entriesByDir := make([]struct {
		dir     string
		entries []os.DirEntry
	}, 0, len(dirs))
	for _, dir := range dirs {
		entries, readErr := os.ReadDir(dir)
		if readErr == nil {
			entriesByDir = append(entriesByDir, struct {
				dir     string
				entries []os.DirEntry
			}{dir: dir, entries: entries})
		}
	}
	if len(entriesByDir) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"schema": "account-node.accounts.v1", "node_id": h.cfg.NodeID,
			"revision": 0, "total": 0, "cursor": nil, "accounts": []any{},
		})
		return
	}

	type row struct {
		key  string
		item map[string]any
	}
	var rows []row
	var revision int64
	seen := map[string]bool{}
	for _, bucket := range entriesByDir {
		for _, e := range bucket.entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			path := filepath.Join(bucket.dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var doc map[string]any
			if json.Unmarshal(raw, &doc) != nil {
				continue
			}
			email := emailFromDocument(doc)
			if !strings.Contains(email, "@") {
				continue
			}
			keyID := strings.ToLower(strings.TrimSpace(email))
			if seen[keyID] {
				continue
			}
			seen[keyID] = true
			info, err := e.Info()
			if err != nil {
				continue
			}
			version := info.ModTime().Unix()
			if version > revision {
				revision = version
			}
			if version <= since {
				continue
			}
			digest := sha256.Sum256(raw)
			keySum := sha256.Sum256([]byte(strings.ToLower(email)))
			rows = append(rows, row{
				key: hex.EncodeToString(keySum[:])[:32],
				item: map[string]any{
					"email":             email,
					"email_key":         hex.EncodeToString(keySum[:])[:32],
					"version":           version,
					"state":             accountState(doc),
					"platform":          h.cfg.Platform,
					"credential_sha256": hex.EncodeToString(digest[:]),
					"last_verified_at":  version,
				},
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })

	start := 0
	if len(after) == 32 {
		want := string(after)
		for i, r := range rows {
			if r.key > want {
				start = i
				break
			}
			start = i + 1
		}
	}
	end := start + limit
	if end > len(rows) {
		end = len(rows)
	}
	page := make([]map[string]any, 0, end-start)
	for _, r := range rows[start:end] {
		page = append(page, r.item)
	}
	var next any
	if end < len(rows) && end > start {
		next = base64.URLEncoding.EncodeToString([]byte(rows[end-1].key))
	}
	log.Printf("[node] accounts report total=%d page=%d revision=%d", len(rows), len(page), revision)
	c.JSON(http.StatusOK, gin.H{
		"schema":   "account-node.accounts.v1",
		"node_id":  h.cfg.NodeID,
		"revision": revision,
		"total":    len(rows),
		"cursor":   next,
		"accounts": page,
	})
}

// accountState 只描述这个账号现在能不能用。
// active: 有 refresh_token 且 access_token 未过期
// stale:  有 refresh_token 但 access_token 已过期（刷新即可恢复）
// unusable: 缺 refresh_token
func accountState(doc map[string]any) string {
	refresh := credentialString(doc, "refresh_token", "refreshToken")
	if refresh == "" {
		return "unusable"
	}
	if exp := tokenExpiry(credentialString(doc, "access_token", "accessToken")); exp > 0 && exp <= time.Now().Unix() {
		return "stale"
	}
	return "active"
}

func credentialString(value any, keys ...string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range keys {
		if candidate, ok := object[key].(string); ok && candidate != "" {
			return candidate
		}
	}
	for _, key := range []string{"payload", "credentials", "oauth", "token", "tokens", "account", "document"} {
		if found := credentialString(object[key], keys...); found != "" {
			return found
		}
	}
	return ""
}

// tokenExpiry 从 JWT 里读 exp，读不到返回 0。
func tokenExpiry(value any) int64 {
	raw, _ := value.(string)
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return 0
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	data, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return 0
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return 0
	}
	if v, ok := claims["exp"].(float64); ok {
		return int64(v)
	}
	return 0
}
