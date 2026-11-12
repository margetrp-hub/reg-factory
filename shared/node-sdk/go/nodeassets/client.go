package nodeassets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Send must use the node's authenticated Work relay. Exchange deliberately
// sends once; only the owning node's durable outbox may retry a saved result.
type Send func(context.Context, Envelope) (Envelope, error)

func Exchange(ctx context.Context, send Send, nodeID, key string, body map[string]any) (map[string]any, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	requestID := hex.EncodeToString(id)
	e, err := Pack(nodeID, key, requestID, "request", body, time.Now())
	if err != nil {
		return nil, err
	}
	response, err := send(ctx, e)
	if err != nil {
		return nil, err
	}
	m, err := Unpack(response, key, nodeID, requestID, "response", time.Now())
	if err != nil {
		return nil, err
	}
	if m.Body["task_id"] != body["task_id"] {
		return nil, ErrScope
	}
	if body["action"] == "result" {
		r, ok := body["result"].(map[string]any)
		if !ok || m.Body["result_id"] != r["result_id"] {
			return nil, ErrScope
		}
	}
	return m.Body, nil
}
