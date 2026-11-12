// Package nodeassets exchanges opaque asset messages, not business operations.
// Work transport keys and the Production storage master key are never reused.
package nodeassets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const Schema = "aishop.node-assets.v1"

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var ErrInvalid = errors.New("ASSET_MESSAGE_INVALID")
var ErrScope = errors.New("ASSET_SCOPE_INVALID")
var ErrAuthentication = errors.New("ASSET_AUTHENTICATION_FAILED")

type Envelope struct {
	Schema string `json:"schema"`
	NodeID string `json:"node_id"`
	Sealed string `json:"sealed"`
}

type Message struct {
	Schema    string         `json:"schema"`
	NodeID    string         `json:"node_id"`
	RequestID string         `json:"request_id"`
	Direction string         `json:"direction"`
	IssuedAt  int64          `json:"issued_at"`
	ExpiresAt int64          `json:"expires_at"`
	Body      map[string]any `json:"body"`
}

func aead(key string) (cipher.AEAD, error) {
	k, err := base64.URLEncoding.Strict().DecodeString(key)
	if err != nil || len(k) != 32 || base64.URLEncoding.EncodeToString(k) != key {
		return nil, ErrAuthentication
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, ErrAuthentication
	}
	return cipher.NewGCM(b)
}

func Pack(nodeID, key, requestID, direction string, body map[string]any, now time.Time) (Envelope, error) {
	if !identifier.MatchString(nodeID) || !identifier.MatchString(requestID) ||
		(direction != "request" && direction != "response") || body == nil {
		return Envelope{}, ErrScope
	}
	m := Message{Schema, nodeID, requestID, direction, now.Unix(), now.Unix() + 120, body}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > 256*1024 {
		return Envelope{}, ErrInvalid
	}
	c, err := aead(key)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, c.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, err
	}
	aad := []byte(Schema + "\n" + nodeID + "\n" + direction)
	sealed := c.Seal(nonce, nonce, raw, aad)
	return Envelope{Schema, nodeID, base64.URLEncoding.EncodeToString(sealed)}, nil
}

func Unpack(e Envelope, key, nodeID, requestID, direction string, now time.Time) (Message, error) {
	if e.Schema != Schema || !identifier.MatchString(e.NodeID) || len(e.Sealed) > 360*1024 {
		return Message{}, ErrInvalid
	}
	c, err := aead(key)
	if err != nil {
		return Message{}, err
	}
	sealed, err := base64.URLEncoding.Strict().DecodeString(e.Sealed)
	if err != nil || len(sealed) < c.NonceSize()+c.Overhead() || base64.URLEncoding.EncodeToString(sealed) != e.Sealed {
		return Message{}, ErrAuthentication
	}
	raw, err := c.Open(nil, sealed[:c.NonceSize()], sealed[c.NonceSize():], []byte(Schema+"\n"+e.NodeID+"\n"+direction))
	if err != nil {
		return Message{}, ErrAuthentication
	}
	if len(raw) > 256*1024 {
		return Message{}, ErrInvalid
	}
	if err = uniqueJSON(raw); err != nil {
		return Message{}, err
	}
	var m Message
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return Message{}, ErrInvalid
	}
	if m.Schema != Schema || m.NodeID != e.NodeID || (nodeID != "" && m.NodeID != nodeID) ||
		(requestID != "" && m.RequestID != requestID) || !identifier.MatchString(m.RequestID) || m.Direction != direction ||
		m.Body == nil || m.IssuedAt < now.Unix()-120 || m.IssuedAt > now.Unix()+30 ||
		m.ExpiresAt <= now.Unix() || m.ExpiresAt > m.IssuedAt+120 {
		return Message{}, ErrScope
	}
	return m, nil
}

// Do not let different decoders disagree on duplicate fields or trailing JSON.
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var read func(int) error
	read = func(depth int) error {
		if depth > 32 {
			return ErrInvalid
		}
		t, err := d.Token()
		if err != nil {
			return ErrInvalid
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return ErrInvalid
					}
					s, ok := k.(string)
					if !ok || seen[s] {
						return ErrInvalid
					}
					seen[s] = true
					if err := read(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := read(depth + 1); err != nil {
						return err
					}
				}
			default:
				return ErrInvalid
			}
			if _, err := d.Token(); err != nil {
				return ErrInvalid
			}
		}
		return nil
	}
	if err := read(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data", ErrInvalid)
	}
	return nil
}
