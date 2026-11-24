package completionplan

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Receipt struct {
	Fingerprint string     `json:"fingerprint"`
	MessageID   string     `json:"messageId"`
	Quote       string     `json:"quote"`
	At          string     `json:"at"`
	Source      string     `json:"source"`
	Evidence    []Evidence `json:"evidence,omitempty"`
}

// Confirmed returns the explicit reply to the most recent controller-generated
// proposal. Text in documents, proxy answers and AI declarations is not a receipt.
// Source identifies the authenticated sender, not necessarily the real human.
func Confirmed(p Plan, messages any) (Receipt, bool) {
	if receipt, ok := presentedConfirmation(p, messages); ok {
		return receipt, true
	}
	return QuotedConfirmation(p, messages)
}
func presentedConfirmation(p Plan, messages any) (Receipt, bool) {
	presented, receipt, ok := PresentedAfter(p, messages, 0)
	return receipt, ok && presented.Fingerprint() == p.Fingerprint()
}

// PresentedAfter resolves the reply against its saved presentation, never a new
// AI candidate. fallback is the saved plan for legacy fingerprint-only rows.
// after is the message count when reconfirmation started; older replies cannot
// resolve a later change, even when the plan is subsequently changed back.
func PresentedAfter(fallback Plan, messages any, after int) (Plan, Receipt, bool) {
	raw, _ := json.Marshal(messages)
	var rows []map[string]any
	if json.Unmarshal(raw, &rows) != nil {
		return Plan{}, Receipt{}, false
	}
rows:
	for i := len(rows) - 1; i >= after && i >= 0; i-- {
		row := rows[i]
		if row["role"] != "user" {
			continue
		}
		quote, _ := row["text"].(string)
		switch strings.TrimSpace(quote) {
		case "A", "1-A", "承認します", "この計画を承認します":
		default:
			if changesConfirmation(quote) {
				return Plan{}, Receipt{}, false
			}
			continue // Unrelated follow-up answers do not revoke an earlier exact reply.
		}
		id, _ := row["id"].(string)
		at, _ := row["at"].(string)
		source, _ := row["source"].(string)
		if id == "" || (source != "screen-session" && source != "pc-token" && source != "cli-key") {
			return Plan{}, Receipt{}, false
		}
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			return Plan{}, Receipt{}, false
		}
		for j := i - 1; j >= 0; j-- {
			if rows[j]["role"] == "user" {
				return Plan{}, Receipt{}, false
			}
			if rows[j]["role"] == "assistant" {
				if unpresented(rows[j]) {
					// An exact reply to a row without a controller presentation (for
					// example a model-written question after approval) is neither a
					// new approval nor a revocation. Keep looking for the reply to
					// the saved presentation; the newer candidate is still compared
					// strictly because AllowsCandidateRewrite sees this reply.
					continue rows
				}
				p := fallback
				if value, exists := rows[j]["completionPlan"]; exists {
					var err error
					p, err = Parse(value)
					if err != nil {
						return Plan{}, Receipt{}, false
					}
				} else if _, err := Parse(p.Record()); err != nil {
					return Plan{}, Receipt{}, false
				}
				if rows[j]["completionProposal"] != p.Fingerprint() {
					return Plan{}, Receipt{}, false
				}
				return p, Receipt{Fingerprint: p.Fingerprint(), MessageID: id, Quote: quote, At: at, Source: source}, true
			}
		}
		return Plan{}, Receipt{}, false
	}
	return Plan{}, Receipt{}, false
}

// unpresented is an assistant row saved without any presentation record. A row
// carrying only part of a presentation remains invalid rather than skipped.
func unpresented(row map[string]any) bool {
	proposal, hasProposal := row["completionProposal"]
	_, hasPlan := row["completionPlan"]
	return !hasPlan && (!hasProposal || proposal == nil || proposal == "")
}

// NeedsUser reports whether an approval of this category waits for the user:
// deleting or otherwise irreversible operations and wider permissions. Scope
// and basis items are recorded and reported, and proceed without a reply.
func NeedsUser(category string) bool {
	return category == "permission" || category == "destructive" || category == "irreversible"
}

// HasUserGate reports whether the plan lists any item the user must approve,
// before implementation or after the diff. Plans without one never wait.
func HasUserGate(p Plan) bool {
	for _, a := range p.Approvals {
		if NeedsUser(a.Category) {
			return true
		}
	}
	return false
}

func RequiresConfirmation(p Plan) bool {
	for _, a := range p.Approvals {
		if a.Phase == "before_implementation" && NeedsUser(a.Category) {
			return true
		}
	}
	return false
}

// Check validates provenance again at filing. A caller cannot forge an approval
// by adding a completionApprovals object or by reusing an older plan fingerprint.
func Check(value, messages, receipt any) (Plan, error) {
	p, err := Parse(value)
	if err != nil {
		return p, err
	}
	if !RequiresConfirmation(p) && receipt == nil {
		return p, nil
	}
	actual, ok := Confirmed(p, messages)
	if !ok {
		return p, fmt.Errorf("実装前の承認事項を同じ対象・前提で明示確認していません")
	}
	raw, _ := json.Marshal(receipt)
	var stored Receipt
	if json.Unmarshal(raw, &stored) != nil || !sameReceipt(stored, actual) {
		return p, fmt.Errorf("保存した計画の了承記録が発言と一致しません")
	}
	return p, nil
}

// CheckPresented is used for a pending semantic change, including plans that
// dropped their user-gated items. Old quotations cannot clear this barrier.
func CheckPresented(value, messages, receipt any, after int) (Plan, error) {
	p, err := Parse(value)
	if err != nil {
		return p, err
	}
	actualPlan, actual, ok := PresentedAfter(p, messages, after)
	raw, _ := json.Marshal(receipt)
	var stored Receipt
	if !ok || actualPlan.Fingerprint() != p.Fingerprint() || json.Unmarshal(raw, &stored) != nil || !sameReceipt(stored, actual) {
		return p, fmt.Errorf("変更した前提への明示回答と保存した了承記録を確認していません")
	}
	return p, nil
}

func sameReceipt(a, b Receipt) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

// QuotedConfirmation carries an already explicit instruction without demanding a
// second response just because a plan has now been written. Every target,
// operation and condition must occur literally in the saved affirmative quote.
func QuotedConfirmation(p Plan, messages any) (Receipt, bool) {
	raw, _ := json.Marshal(messages)
	var rows []map[string]any
	if json.Unmarshal(raw, &rows) != nil {
		return Receipt{}, false
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		id, _ := row["id"].(string)
		byID[id] = row
	}
	evidence := []Evidence{}
	for _, a := range p.Approvals {
		if a.Phase != "before_implementation" || !NeedsUser(a.Category) {
			continue
		}
		claim, ok := quotedApproval(a, byID)
		if !ok || laterUserDecision(rows, claim.MessageID) {
			return Receipt{}, false
		}
		evidence = append(evidence, claim)
	}
	if len(evidence) == 0 {
		return Receipt{}, false
	}
	first := evidence[0]
	return Receipt{Fingerprint: p.Fingerprint(), MessageID: first.MessageID, Quote: first.Quote, At: first.At, Source: first.Source, Evidence: evidence}, true
}

// quotedApproval checks that the approval's evidence is a saved user message
// whose affirmative text names the target, operation and condition.
func quotedApproval(a Approval, byID map[string]map[string]any) (Evidence, bool) {
	if a.Evidence == nil {
		return Evidence{}, false
	}
	claim := a.Evidence
	row := byID[claim.MessageID]
	if row == nil || row["role"] != "user" || row["at"] != claim.At {
		return Evidence{}, false
	}
	source, _ := row["source"].(string)
	if source != "screen-session" && source != "pc-token" && source != "cli-key" {
		return Evidence{}, false
	}
	content, _ := row["text"].(string)
	if claim.Quote == "" || content != claim.Quote || !strings.Contains(claim.Quote, a.Target) || !strings.Contains(claim.Quote, a.Operation) || !strings.Contains(claim.Quote, a.Condition) || !affirmative(claim.Quote) {
		return Evidence{}, false
	}
	if _, err := time.Parse(time.RFC3339Nano, claim.At); err != nil {
		return Evidence{}, false
	}
	return Evidence{MessageID: claim.MessageID, Quote: claim.Quote, At: claim.At, Source: source}, true
}

// laterUserDecision reports a later user message that approves anew or changes
// the decision. A later user instruction can revoke or change the earlier
// decision; an older quotation never overrides a newer message.
func laterUserDecision(rows []map[string]any, messageID string) bool {
	found := false
	for _, later := range rows {
		if later["id"] == messageID {
			found = true
			continue
		}
		if found && later["role"] == "user" {
			laterText, _ := later["text"].(string)
			if laterText == "A" || laterText == "1-A" || laterText == "承認します" || laterText == "この計画を承認します" {
				return true
			}
			if changesConfirmation(laterText) {
				return true
			}
		}
	}
	return false
}
func affirmative(quote string) bool {
	for _, denial := range []string{"ない", "しません", "しなく", "取り消", "取消", "撤回", "未承認", "未確認", "禁止", "するな", "という例", "という文"} {
		if strings.Contains(quote, denial) {
			return false
		}
	}
	for _, yes := range []string{"承認します", "許可します", "進めてください", "進めて欲しい", "進めてほしい", "実行してください", "やってください"} {
		if strings.Contains(quote, yes) {
			return true
		}
	}
	return false
}

// withoutUnchangedStatements removes statements that the plan stays as it is, so
// that they do not read as a change. A qualified "変更しませんが…" stays intact.
func withoutUnchangedStatements(text string) string {
	if strings.Contains(text, "ませんが") {
		return text
	}
	for _, phrase := range []string{"変更しません", "変更せず", "変更はありません", "変更なし", "変えません", "変えず"} {
		text = strings.ReplaceAll(text, phrase, "")
	}
	return text
}

func changesConfirmation(text string) bool {
	text = strings.TrimSpace(text)
	if text == "B" || text == "1-B" {
		return true
	}
	text = withoutUnchangedStatements(text)
	// A reference to another issue's acceptance criteria is not itself a
	// change to this plan's conditions. Keep detecting all other condition
	// mentions, regardless of how the change is phrased.
	if strings.Contains(strings.ReplaceAll(text, "受入条件", ""), "条件") {
		return true
	}
	for _, marker := range []string{"条件を追加", "条件を付", "条件を変", "条件を修正", "条件を厳しく", "条件を外", "条件が違", "条件付き", "条件として", "変更", "訂正", "撤回", "取り消", "取消", "やめ", "ではなく", "しない", "しません", "中止", "止め", "ただし", "但し"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
