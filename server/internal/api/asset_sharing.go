package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"aishop-next/execution-go/nodeassets"
	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/centralasset"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
)

func (s *Server) newSharing() *centralasset.Sharing {
	return &centralasset.Sharing{Store: s.st, Current: s.sharedSnapshot, Detach: s.detachHandoff, Config: func() (centralasset.Config, error) {
		env, err := settings.WorkerEnvironment(s.cfg.EnvFile)
		if err != nil {
			return centralasset.Config{}, err
		}
		return centralasset.FromEnvironment(s.cfg.DataDir, env)
	}}
}

func (s *Server) StartAssetSharing(ctx context.Context) <-chan struct{} { return s.sharing.Run(ctx) }

func (s *Server) sharedSnapshot(ref string, options map[string]any) (map[string]any, error) {
	var email, platform, kind, role, status, liveness, checked string
	doc := map[string]any{}
	if strings.HasPrefix(ref, "mail:") {
		if s.icApp == nil {
			return nil, errors.New("ASSET_MAIL_UNAVAILABLE")
		}
		var err error
		email, platform, kind, role, status, doc, err = s.icApp.ShareSnapshot(ref)
		if err != nil {
			return nil, err
		}
	} else {
		a, err := s.st.GetAccount(ref)
		if err != nil {
			return nil, errors.New("ASSET_SOURCE_NOT_FOUND")
		}
		email, platform, kind, status, liveness, checked = a.Identity, a.Platform, a.Kind, a.Status, a.LivenessStatus, a.LivenessCheckedAt
		asset, e := s.assets.Get(ref, a.Identity)
		if e == nil && asset.Payload != nil {
			for k, v := range asset.Payload {
				doc[k] = v
			}
			if platform == "chatgpt" {
				if current, err := assetstore.OAuthDocument(asset.Payload, email); err == nil {
					doc = current
				}
			}
		}
		// Native adapters normalize known shapes, never derive roles from names.
		if kind == "free" || kind == "team" {
			kind = "oauth"
		}
		for old, key := range map[string]string{"sessionKey": "session_token", "session_key": "session_token", "sessionToken": "session_token", "cookie": "session_token", "sso_token": "sso"} {
			if doc[key] == nil && doc[old] != nil {
				doc[key] = doc[old]
			}
		}
		password, totp, _ := s.st.GetCredential(ref)
		if password != "" {
			doc["password"] = password
		}
		if totp != "" {
			doc["totp_secret"] = totp
		}
		if kind == "imported" {
			kind = "password"
		}
		role = "unknown"
	}
	typ := "account"
	if strings.HasPrefix(ref, "mail:") {
		typ = "mailbox"
	}
	if v, ok := options["role"].(string); ok && v != "" {
		role = v
	}
	usage, _ := options["usage"].(string)
	doc["email"] = strings.ToLower(strings.TrimSpace(email))
	doc = selectHandoffParts(doc, options)
	if options["include_credentials"] == false {
		kind = "material"
	}
	if options["include_materials"] == false && typ == "account" {
		typ = "credential"
	}
	email, _ = doc["email"].(string)
	if liveness == "" {
		liveness = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	return map[string]any{"schema": nodeassets.SnapshotSchema, "source_ref": ref, "source_revision": 1,
		"type": typ, "platform": platform, "kind": kind, "email": email, "role": role, "usage": usage, "document": doc,
		"state": map[string]any{"status": status, "liveness": liveness, "checked_at": checked}}, nil
}

func selectHandoffParts(native, options map[string]any) map[string]any {
	material := map[string]any{}
	credential := map[string]any{}
	for k, v := range native {
		credential[k] = v
	}
	for _, k := range []string{"registered_platforms", "email", "email_account", "email_password", "email_2fa", "platform_account", "platform_password", "platform_2fa", "sms_enabled", "sms_phone", "status",
		"password", "totp_secret", "totp", "chatgpt_password", "chatgpt_totp_secret", "recovery", "username", "host", "port", "folder", "ssl", "source_enabled", "endpoint", "api_key", "client_id", "parent_source_ref"} {
		if v, ok := native[k]; ok {
			material[k] = v
		}
		if k != "email" {
			delete(credential, k)
		}
	}
	out := map[string]any{}
	if options["include_materials"] != false {
		for k, v := range material {
			out[k] = v
		}
	}
	if options["include_credentials"] != false && len(credential) > 0 {
		out["credential_json"] = credential
	}
	if fields, ok := options["fields"].(map[string]any); ok {
		for k, v := range fields {
			if k == "credential_json" && options["include_credentials"] != false || k != "credential_json" && options["include_materials"] != false {
				out[k] = v
			}
		}
	}
	return out
}

func (s *Server) detachHandoff(ref string) error {
	if strings.HasPrefix(ref, "mail:") {
		if s.icApp == nil {
			return errors.New("ASSET_MAIL_UNAVAILABLE")
		}
		return s.icApp.WithdrawShared(ref)
	}
	if err := s.st.WithdrawAccount(ref); err != nil {
		return err
	}
	platform, email, err := s.st.TransferredIdentity(ref)
	if err != nil {
		return err
	}
	return s.assets.Withdraw(ref, platform, email)
}

func (s *Server) shareList(c *gin.Context) {
	view := c.DefaultQuery("view", "current")
	if view != "current" && view != "history" && view != "received" {
		c.JSON(422, gin.H{"error": "ASSET_VIEW_INVALID"})
		return
	}
	if view == "received" {
		received, err := s.sharing.Received()
		if err != nil {
			c.JSON(503, gin.H{"error": "ASSET_RECEIVED_UNAVAILABLE"})
			return
		}
		c.JSON(200, gin.H{"received": received})
		return
	}
	records, err := s.sharing.List()
	if err != nil {
		c.JSON(503, gin.H{"error": "ASSET_SHARING_UNAVAILABLE"})
		return
	}
	if view == "history" {
		c.JSON(200, gin.H{"records": records})
		return
	}
	limit := 25
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}
	query := c.Query("q")
	kind := c.DefaultQuery("type", "all")
	if kind != "all" && kind != "account" && kind != "mailbox" {
		c.JSON(422, gin.H{"error": "ASSET_TYPE_INVALID"})
		return
	}
	candidates := []map[string]any{}
	total := 0
	// Stable concatenation keeps account and mailbox pages bounded, including
	// pages that cross the category boundary. Selection never imports material.
	if kind != "mailbox" {
		accounts, count, e := s.st.LibraryAccounts(query, "", "", offset/limit+1, limit)
		err = e
		total = count
		for _, a := range accounts {
			candidates = append(candidates, map[string]any{"ref": a.Ref, "type": "account", "email": a.Identity, "platform": a.Platform, "kind": a.Kind, "status": a.Status})
		}
	}
	if err == nil && kind != "account" && s.icApp != nil {
		mail, count, e := s.icApp.ShareCandidates(c.Request.Context(), query, limit-len(candidates), max(0, offset-total))
		err = e
		total += count
		for _, item := range mail {
			item["type"] = "mailbox"
			candidates = append(candidates, item)
		}
	}
	if err != nil {
		c.JSON(503, gin.H{"error": "ASSET_SOURCE_UNAVAILABLE"})
		return
	}
	c.JSON(200, gin.H{"records": records, "candidates": candidates, "total": total, "profiles": nodeassets.AssetProfiles()})
}

type shareRequest struct {
	Ref       string         `json:"ref"`
	RequestID string         `json:"request_id"`
	Options   map[string]any `json:"options"`
}

func (s *Server) sharePreview(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req shareRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(422, gin.H{"error": "ASSET_REQUEST_INVALID"})
		return
	}
	entry, err := s.sharing.Preview(req.Ref, req.Options)
	if err != nil {
		c.JSON(409, gin.H{"error": "ASSET_SOURCE_UNAVAILABLE"})
		return
	}
	doc, _ := entry["document"].(map[string]any)
	missing := nodeassets.MissingSnapshotFields(entry)
	present := []string{}
	for k, v := range doc {
		if v != nil && v != "" {
			present = append(present, k)
		}
	}
	// Only names/presence are exposed; no saved or submitted secret is echoed.
	delete(entry, "document")
	c.JSON(200, gin.H{"asset": entry, "present": present, "missing": missing})
}

func (s *Server) shareConsent(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req shareRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(422, gin.H{"error": "ASSET_REQUEST_INVALID"})
		return
	}
	record, err := s.sharing.Consent(req.Ref, req.RequestID, req.Options)
	if err != nil {
		code := "ASSET_SHARE_SAVE_FAILED"
		if strings.HasPrefix(err.Error(), "ASSET_") && !strings.ContainsAny(err.Error(), " \r\n") {
			code = strings.Split(err.Error(), ":")[0]
		}
		c.JSON(409, gin.H{"error": code})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "asset.share", req.Ref, c.ClientIP())
	c.JSON(202, record.Public())
}

func (s *Server) shareSync(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req struct {
		Ref string `json:"ref"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(422, gin.H{"error": "ASSET_REQUEST_INVALID"})
		return
	}
	record, err := s.sharing.Sync(c.Request.Context(), req.Ref)
	if err != nil {
		c.JSON(409, gin.H{"error": record.Error, "record": record.Public()})
		return
	}
	c.JSON(200, record.Public())
}
