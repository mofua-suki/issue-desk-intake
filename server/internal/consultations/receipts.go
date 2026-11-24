package consultations

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/readevidence"
)

const sendIDConflictMessage = "送信IDは別の依頼で使用済みです。"
const sendIDUsedMessage = "送信IDは使用済みです。"
const sendIDConflictCode = "send_id_conflict"

var issueFields = []string{"project", "provider", "consultationProvider", "id", "text", "priority", "priorityReason", "tier", "implementationTier", "replaceIssues", "hold", "notDuplicate", "despitePending", "readEvidence"}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// canonicalJSON follows JSON.stringify for these JSON boundary values, including
// raw U+2028/U+2029. Receipts store the canonical JSON itself, not a hash of it.
func canonicalJSON(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	input := strings.TrimSuffix(buffer.String(), "\n")
	var out strings.Builder
	for i := 0; i < len(input); i++ {
		if input[i] == '\\' && i+1 < len(input) {
			if input[i+1] == '\\' {
				out.WriteString(input[i : i+2])
				i++
				continue
			}
			if i+6 <= len(input) && (input[i:i+6] == `\u2028` || input[i:i+6] == `\u2029`) {
				if input[i+5] == '8' {
					out.WriteRune('\u2028')
				} else {
					out.WriteRune('\u2029')
				}
				i += 5
				continue
			}
		}
		out.WriteByte(input[i])
	}
	return out.String(), nil
}

func (s *Service) validateIssue(data Record) error {
	if !recordID.MatchString(text(data["id"])) {
		return refuse(400, "依頼IDが不正です。")
	}
	if err := s.checkProject(text(data["project"])); err != nil {
		return err
	}
	body, ok := data["text"].(string)
	if !ok || contract.UTF16Length(strings.TrimSpace(body)) < 1 || contract.UTF16Length(body) > 8000 {
		return refuse(400, "依頼内容を1〜8000文字で渡してください。")
	}
	if p, present := data["provider"]; present && p != "codex" && p != "claude" {
		return refuse(400, "実行先はcodexまたはclaudeを指定してください。")
	}
	if _, err := s.provider(text(data["provider"])); err != nil {
		return err
	}
	if _, err := consultationProvider(text(data["consultationProvider"])); err != nil {
		return err
	}
	if err := requireTier(data["tier"]); err != nil {
		return err
	}
	if value, present := data["implementationTier"]; present && value != nil && value != "light" && value != "standard" && value != "heavy" {
		return refuse(400, "難易度はlight・standard・heavyで指定してください。")
	}
	if value, present := data["hold"]; present {
		if _, ok := value.(bool); !ok {
			return refuse(400, "holdは真偽値で指定してください。")
		}
	}
	if value, present := data["priority"]; present && value != nil && value != "high" && value != "normal" {
		return refuse(400, "優先度はhighかnormalを指定してください。")
	}
	if data["priority"] == "high" {
		reason := text(data["priorityReason"])
		if strings.TrimSpace(reason) == "" || len([]rune(strings.TrimSpace(reason))) > 500 {
			return refuse(400, "割り込みの理由を1〜500文字で指定してください。")
		}
	}
	if data["priority"] != "high" {
		if value, present := data["priorityReason"]; present && value != nil {
			return refuse(400, "割り込みの理由はpriorityがhighのときだけ指定します。")
		}
	}
	if reason, present := data["notDuplicate"]; present {
		v, ok := reason.(string)
		if !ok || strings.TrimSpace(v) == "" || len([]rune(strings.TrimSpace(v))) > 500 {
			return refuse(400, "重複でない理由は1〜500文字で指定してください。")
		}
	}
	if values, present := data["replaceIssues"]; present {
		raw, _ := json.Marshal(values)
		var list []json.Number
		if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
			return refuse(400, "置き換えるIssueの配列が不正です。")
		}
		seen := map[int64]bool{}
		for _, v := range list {
			n, err := v.Int64()
			if err != nil || n <= 0 || n > 9007199254740991 || seen[n] {
				return refuse(400, "置き換えるIssueの番号が不正です。")
			}
			seen[n] = true
		}
	}
	if value, present := data["despitePending"]; present {
		raw, _ := json.Marshal(value)
		v, err := decode(string(raw))
		if err != nil || strings.TrimSpace(text(v["draft"])) == "" || strings.TrimSpace(text(v["reason"])) == "" || len([]rune(strings.TrimSpace(text(v["reason"])))) > 500 {
			return refuse(400, "despitePendingには下書きIDと理由を指定してください。")
		}
	}
	return nil
}

func (s *Service) acceptReceipt(ctx context.Context, input Record, kind, draftID, sendID string, p auth.Principal) (Record, error) {
	fingerprint, err := canonicalJSON(Record{"kind": kind, "draftId": draftID, "input": input})
	if err != nil {
		return nil, err
	}
	tx, err := s.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw string
	sender := owner(p)
	err = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE sender=? AND send_id=?"), sender, sendID).Scan(&raw)
	if err == nil {
		old, err := decode(raw)
		if err != nil {
			return nil, err
		}
		if text(old["fingerprint"]) != fingerprint {
			return nil, contract.NewProblem(409, sendIDConflictMessage, sendIDConflictCode)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return old, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	id := draftID
	if kind != "issue" {
		id, err = newID()
		if err != nil {
			return nil, err
		}
	}
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), draftID).Scan(&raw); err == nil {
		current, err := decode(raw)
		if err != nil {
			return nil, err
		}
		if text(current["owner"]) != sender {
			return nil, contract.NewProblem(409, sendIDUsedMessage, sendIDConflictCode)
		}
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	at := timestamp(s.options.Now())
	receipt := Record{"id": id, "kind": kind, "sendId": sendID, "draftId": draftID, "input": input, "fingerprint": fingerprint, "phase": "accepted", "via": "screen", "createdAt": at, "updatedAt": at}
	if sender != "" {
		receipt["owner"] = sender
		receipt["via"] = "token:" + p.Name
	} else if p.Kind == auth.CLI {
		receipt["via"] = "local"
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, bindSQL("INSERT INTO intake_receipts(id,sender,send_id,json) VALUES(?,?,?,?) ON CONFLICT DO NOTHING"), id, sender, sendID, string(encoded))
	if err != nil {
		return nil, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if inserted == 0 {
		// The unique index waits for a competing transaction to finish. Read
		// its committed value in a new statement (READ COMMITTED snapshot).
		err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE sender=? AND send_id=?"), sender, sendID).Scan(&raw)
		if err == sql.ErrNoRows {
			return nil, contract.NewProblem(409, sendIDUsedMessage, sendIDConflictCode)
		}
		if err != nil {
			return nil, err
		}
		receipt, err = decode(raw)
		if err != nil {
			return nil, err
		}
		if text(receipt["fingerprint"]) != fingerprint {
			return nil, contract.NewProblem(409, sendIDConflictMessage, sendIDConflictCode)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (s *Service) AcceptIssue(ctx context.Context, data Record, p auth.Principal) (Record, error) {
	if value, ok := data["readEvidence"]; ok {
		normalized, err := readevidence.Validate(value, text(data["project"]))
		if err != nil {
			return nil, refuse(400, "readEvidenceが不正です。")
		}
		data["readEvidence"] = normalized
	}
	if err := s.validateIssue(data); err != nil {
		return nil, err
	}
	input := Record{}
	for _, key := range issueFields {
		if value, ok := data[key]; ok {
			input[key] = value
		}
	}
	receipt, err := s.acceptReceipt(ctx, input, "issue", text(data["id"]), text(data["id"]), p)
	if err != nil {
		return nil, err
	}
	view, err := s.receiptView(ctx, receipt, p)
	if err != nil {
		return nil, err
	}
	if phase := text(receipt["phase"]); phase == "accepted" || phase == "processing" {
		functions := s.engineSnapshot()
		if functions.Start != nil {
			_, err = functions.Start(ctx, text(receipt["draftId"]), Record{"receipt": receipt}, p)
			if err != nil {
				return nil, err
			}
		}
	}
	return view, nil
}

func (s *Service) receiptView(ctx context.Context, receipt Record, p auth.Principal) (Record, error) {
	if p.Kind == auth.PC && !p.Delegated && text(receipt["owner"]) != owner(p) {
		return nil, refuse(404, "下書きが見つかりません。")
	}
	input, _ := receipt["input"].(map[string]any)
	if input == nil {
		if v, ok := receipt["input"].(Record); ok {
			input = v
		}
	}
	if err := s.checkProject(text(input["project"])); err != nil {
		return nil, err
	}
	draft, found, err := s.options.Records.Get(ctx, "drafts", text(receipt["draftId"]))
	if err != nil {
		return nil, err
	}
	linked := found && text(draft["owner"]) == text(receipt["owner"]) && sent(draft, receipt, input)
	var view Record
	var state string
	if linked {
		view, state = clone(draft), linkedReceiptState(draft)
	} else {
		view, state = unlinkedReceiptView(receipt, input)
	}
	view["receiptId"], view["requestId"], view["draftId"], view["accepted"], view["receiptState"] = receipt["id"], receipt["sendId"], nil, true, state
	if linked {
		view["draftId"] = draft["id"]
	}
	return view, nil
}

// linkedReceiptState derives the receipt state from the draft it created.
func linkedReceiptState(draft Record) string {
	state := "processing"
	switch text(draft["status"]) {
	case "submit_unknown", "submitting":
		state = "unknown"
	case "submitted":
		if draft["issue"] != nil || len(fleet.Items(draft["issues"])) > 0 {
			state = "completed"
		} else {
			state = "rejected"
		}
	case "intake_waiting":
		state = "waiting"
	case "draft":
		if ready, _ := draft["ready"].(bool); ready {
			state = "waiting"
		}
	case "error", "submit_error", "stalled":
		state = "failed"
	}
	return state
}

// unlinkedReceiptView shows a receipt whose draft is not yet created from the receipt alone.
func unlinkedReceiptView(receipt, input Record) (Record, string) {
	state := "processing"
	cp := text(input["consultationProvider"])
	if cp == "" {
		cp = "codex"
	}
	view := Record{"id": receipt["draftId"], "project": input["project"], "provider": input["provider"], "consultationProvider": cp, "status": "thinking", "messages": []any{}, "questions": []any{}, "ready": false, "revision": 0, "createdAt": receipt["createdAt"], "updatedAt": receipt["updatedAt"]}
	if text(receipt["phase"]) == "rejected" {
		state = "rejected"
	} else if text(receipt["phase"]) == "handed" || text(receipt["error"]) != "" {
		state = "failed"
	} else if text(receipt["phase"]) == "accepted" {
		state = "accepted"
	}
	if state == "rejected" || state == "failed" {
		view["status"] = "error"
		view["error"] = receipt["error"]
		view["errorCode"] = receipt["code"]
	}
	if input["hold"] == true {
		view["hold"] = true
	}
	return view, state
}

func sent(draft, receipt Record, input map[string]any) bool {
	if text(draft["owner"]) != text(receipt["owner"]) {
		return false
	}
	if text(receipt["kind"]) == "issue" {
		cp, expected := text(draft["consultationProvider"]), text(input["consultationProvider"])
		if cp == "" {
			cp = "codex"
		}
		if expected == "" {
			expected = "codex"
		}
		if cp != expected {
			return false
		}
	}
	for _, item := range fleet.Items(draft["messages"]) {
		message, _ := item.(map[string]any)
		if message == nil {
			if v, ok := item.(Record); ok {
				message = v
			}
		}
		if text(message["role"]) == "user" && (text(message["id"]) == text(receipt["sendId"]) || text(message["requestId"]) == text(receipt["sendId"])) && text(message["text"]) == strings.TrimSpace(text(input["text"])) {
			if text(receipt["kind"]) == "chat" && text(message["id"]) != text(input["messageId"]) {
				continue
			}
			images := []any{}
			for _, image := range fleet.Items(message["images"]) {
				images = append(images, record(image)["id"])
			}
			expected := fleet.Items(input["images"])
			if expected == nil {
				expected = []any{}
			}
			left, _ := canonicalJSON(images)
			right, _ := canonicalJSON(expected)
			if left != right {
				continue
			}
			return true
		}
	}
	return false
}

func (s *Service) readDraft(ctx context.Context, id string, p auth.Principal) (Record, error) {
	var raw string
	err := s.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE id=?"), id).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		receipt, err := decode(raw)
		if err != nil {
			return nil, err
		}
		d, found, err := s.options.Records.Get(ctx, "drafts", id)
		if err != nil {
			return nil, err
		}
		awaiting := found && text(d["owner"]) == text(receipt["owner"]) && is(text(receipt["phase"]), "accepted", "processing")
		for _, entry := range fleet.Items(d["messages"]) {
			if record(entry)["role"] == "user" {
				awaiting = false
			}
		}
		if !found || text(receipt["draftId"]) == id && (sent(d, receipt, record(receipt["input"])) || awaiting) {
			return s.receiptView(ctx, receipt, p)
		}
	}
	return s.Draft(ctx, id, p)
}
