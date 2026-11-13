// Package centralasset owns SG's durable execution boundary. Business receipts
// and encryption on the wire remain the exported node SDK's responsibility.
package centralasset

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"aishop-next/execution-go/nodeassets"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var errorCode = regexp.MustCompile(`^[A-Z0-9_]{0,95}$`)

type Config struct {
	Directory, NodeID, CenterID, Origin, NodeKey, ReceiptKey, AssetKey, JournalKey string
}

func FromEnvironment(directory string, env []string) (Config, error) {
	v := map[string]string{}
	for _, entry := range env {
		k, value, _ := strings.Cut(entry, "=")
		v[k] = value
	}
	get := func(k string) string { return v["REG_FACTORY_"+k] }
	c := Config{Directory: filepath.Join(directory, "central-assets"), NodeID: get("NODE_ID"), CenterID: get("ASSET_CENTER_ID"),
		Origin: get("WORK_ORIGIN"), NodeKey: get("NODE_KEY"), ReceiptKey: get("NODE_RECEIPT_KEY"), AssetKey: get("ASSET_KEY"), JournalKey: get("ASSET_JOURNAL_KEY")}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.Directory == "" || !identifier.MatchString(c.NodeID) || !identifier.MatchString(c.CenterID) || len(c.NodeKey) < 32 || len(c.ReceiptKey) < 32 {
		return errors.New("ASSET_CONFIGURATION_REQUIRED")
	}
	for _, key := range []string{c.AssetKey, c.JournalKey} {
		raw, err := base64.URLEncoding.Strict().DecodeString(key)
		if err != nil || len(raw) != 32 || base64.URLEncoding.EncodeToString(raw) != key {
			return errors.New("ASSET_CONFIGURATION_REQUIRED")
		}
	}
	if c.AssetKey == c.NodeKey || c.AssetKey == c.ReceiptKey || c.AssetKey == c.JournalKey {
		return errors.New("ASSET_KEY_ISOLATION_REQUIRED")
	}
	u, err := url.Parse(c.Origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || strings.ContainsAny(c.Origin, " \t\r\n") {
		return errors.New("ASSET_WORK_ORIGIN_INVALID")
	}
	return nil
}

func (c Config) VerifyWritable() error {
	if err := c.Validate(); err != nil {
		return err
	}
	for _, name := range []string{"intents", "results"} {
		if err := (journal{filepath.Join(c.Directory, name), c.JournalKey}).verifyWritable(); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) sender() (func(context.Context, map[string]any) (map[string]any, error), error) {
	send, err := nodeassets.WorkSender(c.Origin, c.CenterID, []byte(c.ReceiptKey))
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, body map[string]any) (map[string]any, error) {
		return nodeassets.Exchange(ctx, send, c.NodeID, c.AssetKey, body)
	}, nil
}
