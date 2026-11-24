package consultations

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"sync/atomic"
	"time"
)

// Longer than deployment's HANDOFF_MS=75000. Age alone never displaces a live
// instance: its PostgreSQL transaction holds a liveness lock, not a writer lease.
const receiptTakeoverAfter = 90 * time.Second
const receiptRecoveryInterval = 10 * time.Second

var errReceiptChanged = errors.New("受領の取得印が変わっています")

type receiptClaimKey struct{}
type receiptClaim struct {
	id, instance, at string
	lost             atomic.Bool
}
type receiptEngine struct {
	owner      context.Context // guarded by Engine.mu; supplied by the existing startup owner
	instance   string
	mu         sync.Mutex
	live       *sql.Tx
	liveCancel context.CancelFunc
	scan       sync.Mutex
	loop       sync.Once
	preparing  map[string]bool
}

// Register preparation before touching receipts or Chat. Quiesce and Close must
// wait for it just as they wait for an AI turn and its final database commit.
func (e *Engine) receiptStoppingLocked() bool {
	return e.closing || e.quiescing || e.ctx.Err() != nil || e.receipts.owner != nil && e.receipts.owner.Err() != nil
}

func (e *Engine) beginReceiptWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.receiptStoppingLocked() {
		return false
	}
	e.turns++
	e.wg.Add(1)
	return true
}
func (e *Engine) endReceiptWork() { e.endTurn(); e.wg.Done() }

// Reserve local preparation as well as active AI; otherwise two discovery paths
// in this instance could reuse a stamp before Chat has registered the execution.
func (e *Engine) reserveReceipt(id, draft string) bool {
	e.receipts.mu.Lock()
	defer e.receipts.mu.Unlock()
	if e.receipts.preparing == nil {
		e.receipts.preparing = map[string]bool{}
	}
	if e.receipts.preparing[id] || e.Busy(draft) {
		return false
	}
	e.receipts.preparing[id] = true
	return true
}
func (e *Engine) releaseReceipt(id string) {
	e.receipts.mu.Lock()
	delete(e.receipts.preparing, id)
	e.receipts.mu.Unlock()
}

func (e *Engine) receiptLiveness() error {
	e.receipts.mu.Lock()
	defer e.receipts.mu.Unlock()
	if e.receipts.live != nil {
		if _, err := e.receipts.live.ExecContext(e.ctx, "SELECT 1"); err == nil {
			return nil
		}
		// Never reuse a failed transaction. Reacquire the same instance lock
		// before authorizing claims; a concurrent successor still wins by CAS.
		_ = e.receipts.live.Rollback()
		e.receipts.liveCancel()
		e.receipts.live = nil
	}
	liveCtx, cancel := context.WithCancel(e.ctx)
	tx, err := e.options.Service.options.Records.DB.BeginTx(liveCtx, nil)
	if err != nil {
		cancel()
		return err
	}
	// Schema isolation also prevents test databases and separate applications from
	// sharing a lock. This lock never elects which colour may start an Engine.
	var locked bool
	err = tx.QueryRowContext(e.ctx, bindSQL("SELECT pg_try_advisory_xact_lock(hashtext(current_schema() || ':consultation-engine'), hashtext(?))"), e.receipts.instance).Scan(&locked)
	if err != nil {
		tx.Rollback()
		cancel()
		return err
	}
	if !locked {
		tx.Rollback()
		cancel()
		return errReceiptChanged
	}
	e.receipts.live = tx
	e.receipts.liveCancel = cancel
	go func() {
		defer cancel()
		select {
		case <-liveCtx.Done():
		case <-e.quiesced:
			e.mu.Lock()
			drained := e.drained
			e.mu.Unlock()
			<-drained
		}
		_ = tx.Rollback()
	}()
	return nil
}

func sameReceiptClaim(r Record, c *receiptClaim) bool {
	stamp := record(r["claim"])
	return text(stamp["instanceId"]) == c.instance && text(stamp["acquiredAt"]) == c.at
}

func (e *Engine) claimReceipt(ctx context.Context, expected Record) (Record, *receiptClaim, error) {
	if err := e.receiptLiveness(); err != nil {
		return nil, nil, err
	}
	c := &receiptClaim{id: text(expected["id"]), instance: e.receipts.instance, at: e.options.Service.options.Now().UTC().Format(time.RFC3339Nano)}
	r, err := e.receiptChange(ctx, c.id, func(tx *sql.Tx, current Record) error {
		// A discovery snapshot is a compare-and-swap input, not permission to replace
		// a more recent claim, rejection or completed handoff.
		before, _ := canonicalJSON(expected)
		actual, _ := canonicalJSON(current)
		if before != actual || !is(text(current["phase"]), "accepted", "processing", "handed") {
			return errReceiptChanged
		}
		stamp := record(current["claim"])
		if stamp != nil {
			if text(stamp["instanceId"]) == c.instance {
				c.at = text(stamp["acquiredAt"])
				return nil
			}
			acquired, err := time.Parse(time.RFC3339Nano, text(stamp["acquiredAt"]))
			if err != nil || !e.options.Service.options.Now().After(acquired.Add(receiptTakeoverAfter)) {
				return errReceiptChanged
			}
			// The old process may still be saving after Quiesce or waiting for AI quota.
			// Only an expired claim whose instance lock is gone may be replaced.
			var inactive bool
			if err := tx.QueryRowContext(ctx, bindSQL("SELECT pg_try_advisory_xact_lock(hashtext(current_schema() || ':consultation-engine'), hashtext(?))"), text(stamp["instanceId"])).Scan(&inactive); err != nil {
				return err
			}
			if !inactive {
				return errReceiptChanged
			}
		}
		// Run-based AI checkpoints and proxy saves must lose ownership at the
		// claim commit, not later when the successor obtains its execution slot.
		if err := fenceReceiptRun(ctx, tx, text(current["draftId"])); err != nil {
			return err
		}
		current["claim"] = Record{"instanceId": c.instance, "acquiredAt": c.at}
		current["phase"], current["error"], current["code"] = "processing", nil, nil
		return nil
	})
	return r, c, err
}

func fenceReceiptRun(ctx context.Context, tx *sql.Tx, id string) error {
	var raw string
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), id).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	d, err := decode(raw)
	if err != nil || d["status"] != "thinking" || text(d["run"]) == "" {
		return err
	}
	d["run"] = nil
	encoded, err := canonicalJSON(d)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=? AND json=?"), encoded, id, raw)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errReceiptChanged
	}
	return err
}

// Withdrawal history is committed with deletion. Checking it in CreateDraft's
// transaction also fences a recovery that claimed before Discard committed.
func receiptDraftAvailable(ctx context.Context, tx *sql.Tx, id string) error {
	if ctx.Value(receiptClaimKey{}) == nil {
		return nil
	}
	var withdrawn bool
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT EXISTS(SELECT 1 FROM draft_withdrawals WHERE draft=?)"), id).Scan(&withdrawn); err != nil {
		return err
	}
	if withdrawn {
		return refuse(409, "この相談は破棄またはTODOへ戻しています。")
	}
	return nil
}

// Resolve the receipt for the latest user turn, not merely the draft ID: later
// Chat receipts and legacy drafts must retain their own replay semantics.
func (e *Engine) draftReceipt(ctx context.Context, d Record) (Record, error) {
	messages := fleet.Items(d["messages"])
	for i := len(messages) - 1; i >= 0; i-- {
		m := record(messages[i])
		if m["role"] != "user" {
			continue
		}
		send := text(m["requestId"])
		if send == "" {
			send = text(m["id"])
		}
		var raw string
		err := e.options.Service.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE sender=? AND send_id=?"), text(d["owner"]), send).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		r, err := decode(raw)
		if err != nil {
			return nil, err
		}
		if text(r["draftId"]) == text(d["id"]) && sent(d, r, record(r["input"])) {
			return r, nil
		}
		return nil, nil
	}
	return nil, nil
}

// Lock the receipt until the draft commit. A stamp change can neither slip
// between validation and save nor advance handed independently of that save.
func (e *Engine) changeClaimedDraft(ctx context.Context, id string, finish bool, change Change) (Record, error) {
	c, _ := ctx.Value(receiptClaimKey{}).(*receiptClaim)
	saved, err := e.options.Service.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		var r Record
		var raw string
		if c != nil {
			if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE id=? FOR UPDATE"), c.id).Scan(&raw); err != nil {
				return nil, err
			}
			var err error
			r, err = decode(raw)
			if err != nil {
				return nil, err
			}
			if !sameReceiptClaim(r, c) {
				return nil, errReceiptChanged
			}
		}
		next, err := change(tx, d)
		if err != nil || c == nil || !finish || next == nil || next["status"] == "thinking" {
			return next, err
		}
		r["phase"], r["updatedAt"] = "handed", timestamp(e.options.Service.options.Now())
		encoded, err := canonicalJSON(r)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, bindSQL("UPDATE intake_receipts SET json=? WHERE id=? AND json=?"), encoded, c.id, raw)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, errReceiptChanged
		}
		return next, nil
	})
	if c != nil && errors.Is(err, errReceiptChanged) {
		c.lost.Store(true)
	}
	return saved, err
}

// Recover is called by startup only after the existing writer lease starts the
// exclusive services. Constructing an Engine or binding its functions does not
// start a scanner. Quiesce is the existing owner-loss fence.
func (e *Engine) startReceiptRecovery(ctx context.Context) {
	e.receipts.loop.Do(func() {
		// Owner cancellation fences new preparation immediately through owner.Err;
		// Quiesce retains the existing wait-without-cancellation contract for AI.
		context.AfterFunc(ctx, func() { _ = e.Quiesce(context.Background()) })
		go func() {
			ticker := time.NewTicker(receiptRecoveryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-e.ctx.Done():
					return
				case <-e.quiesced:
					return
				case <-ticker.C:
					if err := e.recoverOnce(ctx); err != nil && ctx.Err() == nil && e.ctx.Err() == nil {
						e.reportStop(e.ctx, "", nil, "受領の再開", err)
					}
				}
			}
		}()
	})
}
