// Read-only release acceptance for credential consolidation. Prints no identities
// or tokens and never starts a worker, upstream request or database migration.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: audit-current DATA_DIRECTORY")
	}
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		panic("invalid data directory")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, "regd.db"))+"?mode=ro")
	if err != nil {
		panic("database unavailable")
	}
	defer db.Close()
	rows, err := db.Query("SELECT ref,identity FROM accounts WHERE platform='chatgpt' ORDER BY ref")
	if err != nil {
		panic("account index unavailable")
	}
	defer rows.Close()
	assets := assetstore.New(root)
	groups := map[string]string{}
	digest := sha256.New()
	count, timed := 0, 0
	var incomplete []map[string]any
	var different []map[string]any
	for rows.Next() {
		var ref, email string
		if rows.Scan(&ref, &email) != nil {
			panic("account index unreadable")
		}
		a, err := assets.Get(ref, email)
		if err != nil {
			panic(fmt.Sprintf("credential unresolved for reference %x", sha256.Sum256([]byte(ref))))
		}
		doc, err := assetstore.OAuthDocument(a.Payload, email)
		if err != nil || doc["refresh_token"] == nil || doc["refresh_token"] == "" {
			keys := []string{}
			for key := range a.Payload {
				keys = append(keys, key)
			}
			incomplete = append(incomplete, map[string]any{"reference_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(ref))), "kind": a.Kind, "payload_keys": keys, "error": fmt.Sprint(err), "has_access_token": doc["access_token"] != nil && doc["access_token"] != ""})
		}
		tokens, _ := json.Marshal([]any{doc["access_token"], doc["refresh_token"], doc["id_token"]})
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(tokens))
		key := strings.ToLower(strings.TrimSpace(email))
		if prior := groups[key]; prior != "" && prior != fingerprint {
			different = append(different, map[string]any{"reference_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(ref))), "kind": a.Kind, "has_refresh_token": doc["refresh_token"] != nil && doc["refresh_token"] != ""})
		}
		groups[key] = fingerprint
		fmt.Fprintf(digest, "%s\n%s\n", ref, fingerprint)
		count++
		if a.CredentialUpdatedAt != "" {
			timed++
		}
	}
	if rows.Err() != nil {
		panic("account index iteration failed")
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"read_only": true, "references": count, "accounts": len(groups), "references_with_save_time": timed, "incomplete": incomplete, "different": different, "credential_fingerprint_sha256": fmt.Sprintf("%x", digest.Sum(nil))})
	if len(incomplete) > 0 || len(different) > 0 {
		os.Exit(1)
	}
}
