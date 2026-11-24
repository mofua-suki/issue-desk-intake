package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"

	"issue-desk-intake/server/internal/auth"
)

func (e *Engine) receipt(ctx context.Context, id string, change func(Record) error) (Record, error) {
	return e.receiptChange(ctx, id, func(_ *sql.Tx, r Record) error { return change(r) })
}
func (e *Engine) receiptChange(ctx context.Context, id string, change func(*sql.Tx, Record) error) (Record, error) {
	tx, err := e.options.Service.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE id=? FOR UPDATE"), id).Scan(&raw); err != nil {
		return nil, err
	}
	r, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if err = change(tx, r); err != nil {
		return nil, err
	}
	r["updatedAt"] = timestamp(e.options.Service.options.Now())
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, bindSQL("UPDATE intake_receipts SET json=? WHERE id=? AND json=?"), string(data), id, raw)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, refuse(409, "受領の記録は更新されています。")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}
func (e *Engine) accept(ctx context.Context, r Record, p auth.Principal) (Record, error) {
	if !e.beginReceiptWork() {
		return nil, refuse(503, "相談の実行処理を停止中です。保存済みの依頼は次回起動時に再開します。")
	}
	defer e.endReceiptWork()
	if !e.reserveReceipt(text(r["id"]), text(r["draftId"])) {
		d, _, err := e.options.Service.options.Records.Get(ctx, "drafts", text(r["draftId"]))
		return d, err
	}
	defer e.releaseReceipt(text(r["id"]))
	claimed, claim, err := e.claimReceipt(ctx, r)
	if errors.Is(err, errReceiptChanged) || err != nil && problem(err).Status == 409 {
		d, _, err := e.options.Service.options.Records.Get(ctx, "drafts", text(r["draftId"]))
		return d, err
	}
	if err != nil {
		return nil, err
	}
	r = claimed
	ctx = context.WithValue(ctx, receiptClaimKey{}, claim)
	service := e.options.Service
	if text(r["owner"]) != "" {
		var err error
		p, err = e.principal(ctx, r)
		if err != nil {
			return e.reject(ctx, r, err)
		}
	}
	input := record(r["input"])
	if err := service.checkProject(text(input["project"])); err != nil {
		return e.reject(ctx, r, err)
	}
	draftID := text(r["draftId"])
	d, _, err := service.options.Records.Get(ctx, "drafts", draftID)
	if err != nil {
		return nil, err
	}
	if sent(d, r, input) {
		if text(d["status"]) == "thinking" {
			return e.Start(ctx, draftID, d, p)
		}
		_, err := e.receipt(ctx, text(r["id"]), func(current Record) error {
			if !sameReceiptClaim(current, claim) {
				return errReceiptChanged
			}
			current["phase"] = "handed"
			return nil
		})
		return d, err
	}
	if text(r["kind"]) == "issue" {
		d, err = service.createReceiptDraft(ctx, input, p)
		if err != nil {
			if problem(err).Status == 503 {
				return d, err
			}
			return e.reject(ctx, r, err)
		}
		// A saved first message can never become a later answer when an initial send
		// is recovered or two clients reuse the same draft identifier.
		for _, entry := range fleet.Items(d["messages"]) {
			if record(entry)["role"] == "user" && !sent(d, r, input) {
				return e.reject(ctx, r, refuse(409, "初回発言は別の送信で保存済みです。"))
			}
		}
	}
	if d == nil {
		return e.reject(ctx, r, refuse(404, "下書きが見つかりません。"))
	}
	messageID := text(input["messageId"])
	if messageID == "" {
		messageID = text(r["sendId"])
	}
	data := clone(input)
	data["id"], data["messageId"], data["revision"] = r["sendId"], messageID, d["revision"]
	d, err = service.Chat(ctx, draftID, data, p)
	if err != nil {
		if problem(err).Status == 503 {
			return d, err
		}
		return e.reject(ctx, r, err)
	}
	return d, nil
}
func (e *Engine) reject(ctx context.Context, r Record, cause error) (Record, error) {
	_, err := e.receipt(ctx, text(r["id"]), func(current Record) error {
		if claim, _ := ctx.Value(receiptClaimKey{}).(*receiptClaim); claim != nil && !sameReceiptClaim(current, claim) {
			return errReceiptChanged
		}
		current["phase"], current["error"] = "rejected", problem(cause).Error()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return nil, cause
}
func (e *Engine) Recover(ctx context.Context) error {
	e.mu.Lock()
	if e.receipts.owner == nil {
		e.receipts.owner = ctx
	}
	owner := e.receipts.owner
	e.mu.Unlock()
	e.startReceiptRecovery(owner)
	return e.recoverOnce(ctx)
}
func (e *Engine) recoverOnce(ctx context.Context) error {
	e.receipts.scan.Lock()
	defer e.receipts.scan.Unlock()
	if !e.beginReceiptWork() {
		return nil
	}
	defer e.endReceiptWork()
	rows, err := e.options.Service.options.Records.DB.QueryContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE "+recoverableReceiptSQL+" ORDER BY "+rowOrder(false)))
	if err != nil {
		return err
	}
	pending := []Record{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		r, err := decode(raw)
		if err != nil {
			rows.Close()
			return err
		}
		if is(text(r["phase"]), "accepted", "processing") {
			pending = append(pending, r)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.Busy(text(r["draftId"])) {
			continue
		}
		if _, err := e.accept(ctx, r, auth.Principal{Kind: auth.Screen}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	entries, err := e.options.Service.options.Records.ListDraftEntries(ctx, DraftReadRecovery)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if text(entry.Record["status"]) == "thinking" && !e.Busy(entry.Key) {
			ids = append(ids, entry.Key)
		}
	}
	drafts, err := e.options.Service.options.Records.GetDrafts(ctx, ids)
	if err != nil {
		return err
	}
	for _, d := range drafts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if text(d["status"]) == "thinking" {
			if _, err := e.Start(ctx, text(d["id"]), d, auth.Principal{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// legacyReceiptTierReason is saved on consultations from receipts accepted
// before the tier became required. They keep the level the removed classifier
// started them at (heavy) so an accepted request is not refused afterwards.
const legacyReceiptTierReason = "難易度の指定を必須にする前に受け付けた依頼のため、難易度「高」で相談します"

func (s *Service) createReceiptDraft(ctx context.Context, input Record, p auth.Principal) (Record, error) {
	if input["tier"] != nil && input["tier"] != "" {
		return s.CreateDraft(ctx, input, p, true)
	}
	legacy := clone(input)
	legacy["tier"] = "heavy"
	d, err := s.CreateDraft(ctx, legacy, p, true)
	if err != nil || d == nil || d["requestedTier"] != "heavy" || record(d["tier"])["source"] != "user" {
		return d, err
	}
	return s.options.Records.Change(ctx, "drafts", text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil {
			return nil, refuse(404, "下書きが見つかりません。")
		}
		tier := clone(record(current["tier"]))
		if tier["source"] == "user" {
			tier["source"], tier["reason"] = "default", legacyReceiptTierReason
		}
		current["tier"] = tier
		return current, nil
	})
}
