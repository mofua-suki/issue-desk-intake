package filingcheck

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// hasNewEvidence compares acquired content and ranges, excluding diagnostics.
func hasNewEvidence(previous, next Record) bool {
	for _, item := range fleet.Elements(next["evidence"]) {
		row := obj(item)
		covered := false
		for _, priorItem := range fleet.Elements(previous["evidence"]) {
			prior := obj(priorItem)
			if row["candidateId"] != prior["candidateId"] || row["kind"] != prior["kind"] || string(raw(row["acquiredRanges"])) != string(raw(prior["acquiredRanges"])) {
				continue
			}
			if !strings.Contains(fleet.Text(prior["body"]), fleet.Text(row["body"])) || !strings.Contains(fleet.Text(prior["reference"]), fleet.Text(row["reference"])) {
				continue
			}
			messages := map[string]bool{}
			for _, msg := range fleet.Elements(prior["messages"]) {
				messages[string(raw(msg))] = true
			}
			includes := true
			for _, msg := range fleet.Elements(row["messages"]) {
				if !messages[string(raw(msg))] {
					includes = false
					break
				}
			}
			if includes {
				covered = true
				break
			}
		}
		if !covered {
			return true
		}
	}
	return false
}

// prioritizeRecent retries the byte budget with newly requested material first.
func prioritizeRecent(all, recent []any) []any {
	keys := map[string]bool{}
	out := append([]any{}, recent...)
	for _, item := range recent {
		r := obj(item)
		keys[string(raw([]any{r["candidateId"], r["kind"], r["selection"]}))] = true
	}
	for _, item := range all {
		r := obj(item)
		if !keys[string(raw([]any{r["candidateId"], r["kind"], r["selection"]}))] {
			out = append(out, item)
		}
	}
	return out
}
func recentOmitted(m Record, recent []any) bool {
	for _, item := range fleet.Elements(m[evidenceOmittedKey]) {
		o := obj(item)
		if o["bodyOmitted"] != true || fleet.WholeNumber(o["limitBytes"]) != 60000 {
			continue
		}
		for _, item := range recent {
			r := obj(item)
			if r["candidateId"] == o["candidateId"] && r["kind"] == o["kind"] && string(raw(r["selection"])) == string(raw(o["selection"])) {
				return true
			}
		}
	}
	return false
}
func omittedMissing(material Record) ([]any, string) {
	var missing []any
	var details []string
	for _, item := range fleet.Elements(material[evidenceOmittedKey]) {
		r := obj(item)
		if r["bodyOmitted"] != true {
			continue
		}
		row := Record{"candidateId": r["candidateId"], "kind": r["kind"], "selection": r["selection"], "reason": r["reason"], "bytes": r["bytes"], "limitBytes": r["limitBytes"]}
		missing = append(missing, row)
		details = append(details, fmt.Sprintf("候補%s・資料%v bytes・上限%v bytes", fleet.Text(r["candidateId"]), r["bytes"], r["limitBytes"]))
	}
	return missing, strings.Join(details, "、")
}

// projectPreviousRefusal compresses only the transient input and leaves the
// persisted hold, its reason, and its metadata unchanged.
func projectPreviousRefusal(material Record) Record {
	out := clone(material)
	hold := obj(out["previousRefusal"])
	if hold == nil {
		return out
	}
	alternatives := fleet.Elements(hold["alternatives"])
	if len(alternatives) == 0 {
		return out
	}
	ids := make([]any, 0, len(alternatives))
	for i, item := range alternatives {
		row := obj(item)
		if row == nil {
			ids = append(ids, Record{"index": i})
			continue
		}
		id := Record{"index": i}
		for _, key := range []string{"id", "number", "issue", "reason", "fingerprint", "revision", "codeRevision", "at"} {
			if v, ok := row[key]; ok {
				id[key] = v
			}
		}
		ids = append(ids, id)
	}
	hold["alternatives"] = Record{"count": len(alternatives), "items": ids, "source": "filingCheckHold.alternatives", "note": "省略した旧候補の詳細は保存元を参照する未確認情報です。"}
	return out
}

// A successful replacement is consumed once, so unrelated failed selections remain visible.
func unresolvedSelections(old, fresh []any, material Record) []any {
	available := map[string]int{}
	for _, item := range fresh {
		request := obj(item)
		for _, item := range fleet.Elements(material["evidence"]) {
			row := obj(item)
			if row["candidateId"] == request["candidateId"] && row["kind"] == request["kind"] && string(raw(row["selection"])) == string(raw(request["selection"])) {
				available[string(raw([]any{request["candidateId"], request["kind"]}))]++
				break
			}
		}
	}
	result := []any{}
	for _, item := range old {
		row := obj(item)
		key := string(raw([]any{row["candidateId"], row["kind"]}))
		if available[key] > 0 {
			available[key]--
		} else {
			result = append(result, item)
		}
	}
	return result
}

// unfitEvidence identifies retained material when even the minimal input is too large.
func unfitEvidence(material Record, recent []any) ([]any, string) {
	var missing []any
	var details []string
	for _, item := range fleet.Elements(material["evidence"]) {
		row := obj(item)
		for _, item := range recent {
			r := obj(item)
			if row["candidateId"] == r["candidateId"] && row["kind"] == r["kind"] && string(raw(row["selection"])) == string(raw(r["selection"])) {
				size := len(raw(row))
				missing = append(missing, Record{"candidateId": r["candidateId"], "kind": r["kind"], "selection": r["selection"], "reason": "入力上限", "bytes": size, "limitBytes": inputLimit})
				details = append(details, fmt.Sprintf("候補%s・資料%d bytes・上限%d bytes", fleet.Text(r["candidateId"]), size, inputLimit))
				break
			}
		}
	}
	return missing, strings.Join(details, "、")
}

func missingBudgetDetails(items []any) string {
	var details []string
	for _, item := range items {
		row := obj(item)
		if row["bytes"] != nil && row["limitBytes"] != nil {
			details = append(details, fmt.Sprintf("候補%s・資料%v bytes・上限%v bytes", fleet.Text(row["candidateId"]), row["bytes"], row["limitBytes"]))
		}
	}
	return strings.Join(details, "、")
}

// inputLimitOnlyMissing reports whether every unresolved request was dropped
// solely by an input size limit, which a resend cannot fix.
func inputLimitOnlyMissing(items []any) bool {
	for _, item := range items {
		row := obj(item)
		if row == nil || fleet.Text(row["candidateId"]) == "" || row["reason"] != omissionInputLimitReason {
			return false
		}
	}
	return len(items) > 0
}
