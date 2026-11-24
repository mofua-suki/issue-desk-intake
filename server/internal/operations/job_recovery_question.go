package operations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/fleet"
)

// Capture values before the judge runs. Keeping a map would let a caller mutate
// the original approval or acceptance criteria while the judgment is in flight.
func recoveryTargetSnapshot(detail map[string]any) (string, error) {
	values := map[string]any{}
	for _, key := range []string{"project", "number", "title", "body", "comments", "result", "completionPlan", "completionApproval", "completionPlanEvidence", "generation"} {
		values[key] = detail[key]
	}
	b, err := json.Marshal(values)
	return string(b), err
}

func recoveryTargetUnchanged(snapshot string, fresh map[string]any) bool {
	next, err := recoveryTargetSnapshot(fresh)
	return err == nil && snapshot == next
}

type recoveryAnswerSource struct {
	Source string `json:"source"`
	Index  *int   `json:"index,omitempty"`
	Quote  string `json:"quote"`
}

type recoveryAnswerBasis struct {
	Kind       string                 `json:"kind"`
	Sources    []recoveryAnswerSource `json:"sources"`
	ApprovalID string                 `json:"approvalId,omitempty"`
	Conditions []recoveryAnswerSource `json:"conditions,omitempty"`
}

func recoverySourceQuote(detail map[string]any, source recoveryAnswerSource) bool {
	quote := strings.TrimSpace(source.Quote)
	if quote == "" || len(quote) > 4000 {
		return false
	}
	var body string
	switch source.Source {
	case "body":
		if source.Index != nil {
			return false
		}
		body = fleet.Text(detail["body"])
	case "comment":
		comments := fleet.List(detail["comments"])
		if source.Index == nil || *source.Index < 0 || *source.Index >= len(comments) {
			return false
		}
		body = fleet.Text(fleet.Object(comments[*source.Index])["body"])
	default:
		return false
	}
	return strings.Contains(body, source.Quote)
}

// A recovery reply is a grounded explanation, never a new approval. The judge
// must identify exact supplied sources; an arbitrary free-form evidence claim
// is insufficient to make an implementation continue.
func recoveryQuestionAnswer(detail, action map[string]any) (bool, string) {
	if !incidentText(action["text"], 8000) {
		return false, "代理回答は既存の送信経路に合わせて8000文字以内にしてください。"
	}
	if detail["status"] != "waiting" {
		return false, "実装の回答待ちではありません。"
	}
	var basis recoveryAnswerBasis
	decoder := json.NewDecoder(strings.NewReader(fleet.Text(action["evidence"])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&basis); err != nil {
		return false, "代理回答の資料と引用を確認できません。"
	}
	if decoder.Decode(new(any)) != io.EOF || len(basis.Sources) < 1 || len(basis.Sources) > 12 {
		return false, "代理回答の根拠が不正です。"
	}
	for _, source := range basis.Sources {
		if !recoverySourceQuote(detail, source) {
			return false, "代理回答の引用が現在の資料にありません。"
		}
	}
	answer := fleet.Text(action["text"])
	for _, declaration := range []string{"承認します", "許可します", "承認しました", "許可しました", "approve permissions", "grant permission"} {
		if strings.Contains(strings.ToLower(answer), declaration) {
			return false, "代理回答から新しい承認や許可を付与できません。"
		}
	}
	switch basis.Kind {
	case "fact", "convention":
		if basis.ApprovalID != "" || len(basis.Conditions) != 0 {
			return false, "事実の回答に承認の情報を混在できません。"
		}
		return true, ""
	case "pastDecision":
		return recoveryPastDecision(detail, basis)
	default:
		return false, "利用者の新しい判断を代理回答できません。"
	}
}

func recoveryPastDecision(detail map[string]any, basis recoveryAnswerBasis) (bool, string) {
	plan, err := completionplan.Parse(detail["completionPlan"])
	if err != nil || basis.ApprovalID == "" {
		return false, "過去判断の記録を確認できません。"
	}
	receipt := fleet.Object(detail["completionApproval"])
	if receipt["fingerprint"] != plan.Fingerprint() || fleet.Text(receipt["messageId"]) == "" || fleet.Text(receipt["quote"]) == "" || fleet.Text(receipt["at"]) == "" {
		return false, "本人の記録済みの事前判断を照合できません。"
	}
	bodyHash := sha256.Sum256([]byte(fleet.Text(detail["body"])))
	if fleet.Object(detail["completionPlanEvidence"])["bodySHA256"] != hex.EncodeToString(bodyHash[:]) {
		return false, "事前判断の対象と現在の依頼本文が一致しません。"
	}
	for _, approval := range plan.Approvals {
		if approval.ID != basis.ApprovalID {
			continue
		}
		if approval.Phase != "before_implementation" {
			return false, "差分確定後の最終承認を代理回答できません。"
		}
		if !recoveryDecisionFromUser(receipt, approval) {
			return false, "参照した判断の本人発言を確認できません。"
		}
		condition := strings.TrimSpace(approval.Condition)
		// Approval conditions have to be observed in the current supplied record,
		// not merely repeated in the old plan or asserted by the judge.
		if condition == "" || len(basis.Conditions) != 1 || strings.TrimSpace(basis.Conditions[0].Quote) != condition || !recoverySourceQuote(detail, basis.Conditions[0]) {
			return false, "過去判断の前提と現在の資料の一致を確認できません。"
		}
		return true, ""
	}
	return false, "参照された過去判断がありません。"
}

func recoveryDecisionFromUser(receipt map[string]any, approval completionplan.Approval) bool {
	raw, present := receipt["evidence"]
	if !present {
		// A direct response to the complete controller proposal covers its terms.
		return receipt["source"] == "screen-session"
	}
	evidence, valid := raw.([]any)
	if !valid || len(evidence) == 0 {
		return false
	}
	// A carried plan can contain quotations from different authenticated senders.
	// The receipt's top-level sender only describes its first quote, so verify the
	// particular approval rather than treating every later quote as human-origin.
	if approval.Evidence == nil {
		return false
	}
	for _, raw := range evidence {
		item := fleet.Object(raw)
		if item["messageId"] == approval.Evidence.MessageID && item["quote"] == approval.Evidence.Quote && item["at"] == approval.Evidence.At && item["source"] == "screen-session" {
			return true
		}
	}
	return false
}
