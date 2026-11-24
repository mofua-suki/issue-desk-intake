package consultations

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

const filingCheckSummaryLimit = 20

var filingCheckResults = map[string]bool{"refused": true, "evidence_requested": true, "ai_failed": true, "invalid_answer": true, "allowed": true}

// filingCheckSummary は保存済みの判定履歴から、読み手へ出す項目だけを選ぶ。
// 欠けた結果・理由は nil のままにし、lastRefusal などで補わない。
func filingCheckSummary(d Record) Record {
	calls := record(d["filingCheckCalls"])
	if calls == nil {
		return nil
	}
	saved := fleet.Items(calls["checks"])
	if len(saved) > filingCheckSummaryLimit {
		saved = saved[len(saved)-filingCheckSummaryLimit:]
	}
	checks := make([]any, 0, len(saved))
	for _, item := range saved {
		row := record(item)
		if row == nil {
			continue
		}
		entry := Record{"revision": row["revision"], "phase": row["phase"], "at": nil, "result": nil, "reason": nil}
		if at := text(row["at"]); at != "" {
			entry["at"] = at
		}
		if result := text(row["result"]); filingCheckResults[result] {
			entry["result"] = result
		}
		if reason := text(row["reason"]); strings.TrimSpace(reason) != "" {
			entry["reason"] = contract.LimitCause(contract.MaskCredentials(reason))
		}
		checks = append(checks, entry)
	}
	return Record{"revision": calls["revision"], "count": calls["count"], "checks": checks}
}
