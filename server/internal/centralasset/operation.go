package centralasset

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"aishop-next/execution-go/nodeassets"
	"github.com/google/uuid"
)

type Executor func(context.Context, map[string]any) (map[string]any, error)

type Operation struct {
	scope            map[string]any
	id               string
	intents, results journal
	claim            func(context.Context) (map[string]any, error)
	send             func(context.Context, map[string]any) (map[string]any, error)
	// Progress contains only fixed stage names, never material or tokens.
	Progress func(string)
	// Restore only performs idempotent local installation of retained material.
	Restore Executor
}

func New(c Config, scope map[string]any) (*Operation, error) {
	var err error
	c, err = forScope(c, scope)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	id, ok := scope["attempt_id"].(string)
	account, accountOK := scope["subject_id"].(string)
	action := scope["action"]
	expected := map[string]any{"asset_ref": map[string]any{"center_id": c.CenterID}}
	if !ok || !accountOK || !identifier.MatchString(account) || scope["node_id"] != c.NodeID || !equal(scope["parameters"], expected) ||
		(action != "asset.liveness" && action != "account.refresh" && action != "account.authorize" && action != "asset.restore") || number(scope["resource_version"]) < 1 || number(scope["expires_at"]) < 1 {
		return nil, errors.New("ASSET_SCOPE_INVALID")
	}
	op := &Operation{scope: scope, id: id, intents: journal{filepath.Join(c.Directory, "intents"), c.JournalKey}, results: journal{filepath.Join(c.Directory, "results"), c.JournalKey}}
	if _, err := op.results.path(id); err != nil {
		return nil, err
	}
	op.send, err = c.sender()
	op.claim = func(ctx context.Context) (map[string]any, error) { return c.claim(ctx, id) }
	return op, err
}

func (o *Operation) progress(stage string) {
	if o.Progress != nil {
		o.Progress(stage)
	}
}

func (o *Operation) saved() (map[string]any, error) {
	v, err := o.results.get(o.id)
	if err != nil || v == nil {
		return v, err
	}
	if !equal(v["context"], o.scope) {
		return nil, errors.New("ASSET_TASK_SCOPE_CHANGED")
	}
	m, ok := v["message"].(map[string]any)
	r, rok := m["result"].(map[string]any)
	result, executorOK := v["result"].(map[string]any)
	_, preflightOK := v["preflight"].(bool)
	if !ok || !rok || !executorOK || !preflightOK || m["action"] != "result" || m["task_id"] != o.id || len(r) != 6 ||
		!identifier.MatchString(str(r["result_id"])) || !errorCode.MatchString(str(result["error_code"])) ||
		(r["outcome"] != "success" && r["outcome"] != "failed" && r["outcome"] != "unknown") {
		return nil, errors.New("ASSET_JOURNAL_INVALID")
	}
	return v, nil
}

func str(v any) string { s, _ := v.(string); return s }

func (o *Operation) expectation(value map[string]any) nodeassets.ResultExpectation {
	r := value["message"].(map[string]any)["result"].(map[string]any)
	return nodeassets.ResultExpectation{TaskID: o.id, AccountID: str(o.scope["subject_id"]), ResultID: str(r["result_id"]), Outcome: str(r["outcome"]),
		ExpectedRevision: number(o.scope["resource_version"]), Refresh: o.scope["action"] == "account.refresh" || o.scope["action"] == "account.authorize"}
}

func (o *Operation) stage(result map[string]any, preflight bool) error {
	old, err := o.saved()
	if err != nil || old != nil {
		return err
	}
	outcome := map[string]string{"completed": "success", "failed": "failed", "unknown": "unknown"}[str(result["status"])]
	code, codeOK := result["error_code"].(string)
	if outcome == "" || !codeOK || !errorCode.MatchString(code) {
		return errors.New("ASSET_EXECUTOR_RESULT_INVALID")
	}
	var document any
	if result["status"] == "completed" && (o.scope["action"] == "account.refresh" || o.scope["action"] == "account.authorize" || o.scope["action"] == "asset.restore") {
		var ok bool
		document, ok = result["document"].(map[string]any)
		if !ok || document == nil {
			return errors.New("ASSET_EXECUTOR_RESULT_INVALID")
		}
	}
	observation, err := nodeassets.ObserveResult(result)
	if err != nil {
		return err
	}
	message := map[string]any{"action": "result", "task_id": o.id, "result": map[string]any{
		"result_id": uuid.NewString(), "outcome": outcome, "document": document,
		"source_status": observation.SourceStatus, "source_liveness": observation.SourceLiveness, "checked_at": observation.CheckedAt}}
	return o.results.put(o.id, map[string]any{"task_id": o.id, "context": o.scope, "message": message, "preflight": preflight, "result": result})
}

func failure(code string, invoked bool) map[string]any {
	status := "failed"
	if invoked {
		status = "unknown"
	}
	return map[string]any{"status": status, "document": nil, "error_code": code}
}

// Run invokes the platform at most once, after a durable intent. Neither a
// restarted process nor a repeated submission is allowed to repeat that call.
func (o *Operation) Run(ctx context.Context, execute Executor) (map[string]any, error) {
	if v, err := o.saved(); err != nil || v != nil {
		if err != nil {
			return nil, err
		}
		return o.Deliver(ctx)
	}
	if intent, err := o.intents.get(o.id); err != nil || intent != nil {
		if err != nil {
			return nil, err
		}
		return o.Replay(ctx, true)
	}
	invoked, result := false, map[string]any(nil)
	perform := func() error {
		if time.Now().Unix() >= number(o.scope["expires_at"]) {
			return errors.New("WORK_AUTHORIZATION_EXPIRED")
		}
		if err := o.results.verifyWritable(); err != nil {
			return err
		}
		if err := o.intents.verifyWritable(); err != nil {
			return err
		}
		o.progress("asset_claim")
		r, err := o.claim(ctx)
		if err != nil {
			return err
		}
		if r["task_id"] != o.id || r["account_id"] != o.scope["subject_id"] || r["node_id"] != o.scope["node_id"] || r["state"] != "claimed" || number(r["expected_revision"]) != number(o.scope["resource_version"]) {
			return errors.New("ASSET_CLAIM_RECEIPT_INVALID")
		}
		o.progress("asset_read")
		material, err := o.send(ctx, map[string]any{"action": "read", "task_id": o.id})
		if err != nil {
			return err
		}
		if material["account_id"] != o.scope["subject_id"] || number(material["credential_revision"]) != number(o.scope["resource_version"]) {
			return errors.New("ASSET_READ_SCOPE_INVALID")
		}
		if ctx.Err() != nil {
			return errors.New("NODE_TASK_NOT_STARTED")
		}
		if time.Now().Unix() >= number(o.scope["expires_at"]) {
			return errors.New("WORK_AUTHORIZATION_EXPIRED")
		}
		intent := map[string]any{"task_id": o.id, "context": o.scope}
		if o.scope["action"] == "asset.restore" {
			intent["material"] = material
		}
		if err = o.intents.put(o.id, intent); err != nil {
			return err
		}
		invoked = true
		o.progress("asset_execution")
		if o.scope["action"] == "asset.restore" {
			if o.Restore == nil {
				return errors.New("ASSET_RESTORE_RUNTIME_UNAVAILABLE")
			}
			result, err = o.Restore(ctx, material)
		} else {
			result, err = execute(ctx, material)
		}
		if err != nil {
			return err
		}
		o.progress("asset_result_save")
		return o.stage(result, false)
	}
	if err := perform(); err != nil {
		if invoked && o.scope["action"] == "asset.restore" {
			return nil, err
		}
		code := err.Error()
		if !errorCode.MatchString(code) || code == "" {
			code = "ASSET_PREPARATION_FAILED"
			if invoked {
				code = "ASSET_EXECUTION_UNKNOWN"
			}
		}
		f := failure(code, invoked)
		if result != nil {
			f["unapplied_result"] = result
		}
		if err = o.stage(f, !invoked); err != nil {
			return nil, err
		}
	}
	// Cancellation stops execution, not the durable result delivery. Run returns
	// pending promptly; the normal recovery loop retries with a fresh context.
	return o.Deliver(ctx)
}

func (o *Operation) Replay(ctx context.Context, started bool) (map[string]any, error) {
	v, err := o.saved()
	if err != nil {
		return nil, err
	}
	if v == nil {
		intent, err := o.intents.get(o.id)
		if err != nil {
			return nil, err
		}
		if intent != nil && !equal(intent["context"], o.scope) {
			return nil, errors.New("ASSET_TASK_SCOPE_CHANGED")
		}
		if intent != nil && o.scope["action"] == "asset.restore" {
			material, ok := intent["material"].(map[string]any)
			if !ok || o.Restore == nil {
				return nil, errors.New("ASSET_RESTORE_RUNTIME_UNAVAILABLE")
			}
			// Manual closure prevents local installation from resuming. A failed
			// read is retryable and cannot grant new authority.
			receipt, e := o.send(ctx, map[string]any{"action": "receipt", "task_id": o.id})
			if e != nil {
				return nil, e
			}
			if receipt["state"] == "closed" {
				if e = o.stage(failure("ASSET_CHECKOUT_CLOSED", true), false); e != nil {
					return nil, e
				}
				return o.Deliver(ctx)
			}
			if receipt["task_id"] != o.id || receipt["state"] != "claimed" {
				return nil, errors.New("ASSET_RESTORE_RECEIPT_PENDING")
			}
			result, e := o.Restore(ctx, material)
			if e != nil {
				return nil, e
			}
			if e = o.stage(result, false); e != nil {
				return nil, e
			}
			return o.Deliver(ctx)
		}
		code, invoked := "NODE_TASK_NOT_STARTED", started || intent != nil
		if intent != nil {
			code = "NODE_EXECUTION_INTERRUPTED"
		} else if started {
			code = "NODE_EXECUTION_EVIDENCE_MISSING"
		}
		if err = o.stage(failure(code, invoked), !invoked); err != nil {
			return nil, err
		}
	}
	return o.Deliver(ctx)
}

func (o *Operation) Deliver(ctx context.Context) (map[string]any, error) {
	v, err := o.saved()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errors.New("ASSET_RESULT_NOT_AVAILABLE")
	}
	o.progress("asset_writeback")
	expected := o.expectation(v)
	receipt, err := o.send(ctx, v["message"].(map[string]any))
	state := "pending"
	if err == nil {
		state, err = nodeassets.ValidateResultReceipt(receipt, expected)
	}
	if err != nil {
		receipt, err = o.send(ctx, map[string]any{"action": "receipt", "task_id": o.id})
		if err == nil {
			if v["preflight"] == true && equal(receipt, map[string]any{"task_id": o.id, "state": "absent"}) {
				state = "failed"
			} else {
				state, err = nodeassets.ValidateResultReceipt(receipt, expected)
			}
		}
	}
	code := str(v["result"].(map[string]any)["error_code"])
	if err != nil {
		state, receipt, code = "pending", map[string]any{}, "ASSET_RESULT_DELIVERY_PENDING"
	} else if state == "closed" {
		code = "ASSET_CHECKOUT_CLOSED"
	}
	revision := receipt["credential_revision"]
	if revision == nil {
		revision = o.scope["resource_version"]
	}
	r := v["message"].(map[string]any)["result"].(map[string]any)
	return map[string]any{"task_id": o.id, "account_id": expected.AccountID, "result_id": expected.ResultID,
		"state": state, "error_code": code, "outcome": expected.Outcome, "expected_revision": o.scope["resource_version"],
		"credential_revision": revision, "receipt": receipt, "preflight": v["preflight"],
		"observation": map[string]any{"source_status": r["source_status"], "source_liveness": r["source_liveness"], "checked_at": r["checked_at"]}}, nil
}

// Acknowledge is called only after the local terminal receipt transaction.
func (o *Operation) Acknowledge(receipt map[string]any) error {
	v, err := o.saved()
	if err != nil || v == nil {
		return err
	}
	if !(v["preflight"] == true && equal(receipt, map[string]any{"task_id": o.id, "state": "absent"})) {
		state, err := nodeassets.ValidateResultReceipt(receipt, o.expectation(v))
		if err != nil {
			return err
		}
		if state != "completed" && state != "failed" && state != "closed" {
			return errors.New("ASSET_RESULT_NOT_TERMINAL")
		}
	}
	intent, err := o.intents.get(o.id)
	if err != nil {
		return err
	}
	if intent != nil {
		if !equal(intent["context"], o.scope) {
			return errors.New("ASSET_TASK_SCOPE_CHANGED")
		}
		if err = o.intents.acknowledge(o.id, intent); err != nil {
			return err
		}
	}
	return o.results.acknowledge(o.id, v)
}
