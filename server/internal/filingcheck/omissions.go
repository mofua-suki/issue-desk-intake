package filingcheck

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// Shared external representation of evidence omitted due to the input budget.
const (
	evidenceOmittedKey       = "evidenceOmitted"
	omissionCandidateIDKey   = "candidateId"
	omissionBodyOmittedKey   = "bodyOmitted"
	omissionReasonKey        = "reason"
	omissionInputLimitReason = "資料入力上限"
)

// inputOmissions records what this particular AI call could not see.
func inputOmissions(material Record) []any {
	out := []any{}
	for _, item := range fleet.Elements(material["omittedCandidateSummaries"]) {
		row := clone(obj(item))
		row["reason"], row["kind"] = "入力上限", "候補本文・発言"
		if strings.HasPrefix(fleet.Text(row["candidateId"]), "request:") || strings.HasPrefix(fleet.Text(row["candidateId"]), "job:") {
			row["bodyRequiredForDuplicateCheck"] = true
		}
		out = append(out, row)
	}
	dc := obj(obj(material["target"])["decisionContext"])
	for _, item := range fleet.Elements(dc["references"]) {
		ref := obj(item)
		if n := fleet.WholeNumber(ref["messageCount"]); n > 0 && ref["messages"] == nil {
			out = append(out, Record{"candidateId": "target", "draftId": ref["draftId"], "kind": "祖先発言", "count": n, "reason": "入力上限"})
		}
	}
	for _, item := range fleet.Elements(material[evidenceOmittedKey]) {
		out = append(out, clone(obj(item)))
	}
	// 入力上限へ収めるために落とした要約の詳細・候補。起票済み依頼を一覧から外したときは本文での重複照合が要る。
	for _, item := range fleet.Elements(material[inputLimitOmittedKey]) {
		row := clone(obj(item))
		delete(row, "note")
		if row["kind"] == "候補一覧" {
			for _, id := range fleet.Elements(row["candidateIds"]) {
				if strings.HasPrefix(fleet.Text(id), "request:") || strings.HasPrefix(fleet.Text(id), "job:") {
					row["bodyRequiredForDuplicateCheck"] = true
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// 起票済み依頼の目的は本文で照合する。件数上限は要求を絞れるため例外にしない。
// 未起票の相談では、reference要求の資料全体が入力上限で省略され本文も見えない場合も、
// body要求と同様に省略部分を未確認のまま、見えた範囲で重複判定を続ける。
// 根拠は evidence.go の Service.expand が資料全体の省略時に evidenceOmitted へ記録する
// candidateId、bodyOmitted=true、reason=資料入力上限 であり、参照・発言だけの省略とは区別する。
func omittedDuplicateBody(material Record, id string) bool {
	if !strings.HasPrefix(id, "consultation:") || candidates(material)[id] == nil {
		return false
	}
	for _, item := range fleet.Elements(material["evidenceMissing"]) {
		if obj(item)["candidateId"] == id {
			return false
		}
	}
	for _, item := range fleet.Elements(material[evidenceOmittedKey]) {
		row := obj(item)
		if row[omissionCandidateIDKey] == id && row[omissionBodyOmittedKey] == true && row[omissionReasonKey] == omissionInputLimitReason {
			return true
		}
	}
	return false
}

// refusalCategory records the reported classification; code-enforced stops are marked other.
// Unreported and inconsistent classifications are distinguished and never change the verdict.
func refusalCategory(decision Record, omissions []any) string {
	if decision["proceed"] != false {
		return "unknown"
	}
	switch fleet.Text(decision["refusalCategory"]) {
	case "other":
		return "other"
	case "", "unknown":
		return "unreported"
	case "input_limit_only":
		for _, item := range omissions {
			row := obj(item)
			id := fleet.Text(row["candidateId"])
			filedBody := row[omissionBodyOmittedKey] == true && (strings.HasPrefix(id, "request:") || strings.HasPrefix(id, "job:"))
			if row["bodyRequiredForDuplicateCheck"] != true && !filedBody && (row["reason"] == "入力上限" || row[omissionReasonKey] == omissionInputLimitReason) {
				return "input_limit_only"
			}
		}
	}
	return "unknown"
}
