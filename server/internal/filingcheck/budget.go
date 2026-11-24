package filingcheck

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
)

const filingEvidenceLimit = 8
const filingDecisionLimit = 5

// Pending reservations are charged as decisions until a known evidence result arrives.
type filingBudget struct {
	Evidence  int  `json:"evidence"`
	Decisions int  `json:"decisions"`
	Pending   bool `json:"pending"`
}

func budgetInteger(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n >= 0
	case int64:
		return int(n), n >= 0 && int64(int(n)) == n
	case float64:
		return int(n), n >= 0 && n <= 1000000 && float64(int(n)) == n
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil && i >= 0 && int64(int(i)) == i
	}
	return 0, false
}

// A code revision change alone never resets the budget.
func loadFilingBudget(calls Record, revision any) filingBudget {
	if calls == nil {
		return filingBudget{}
	}
	exhausted := filingBudget{Evidence: filingEvidenceLimit, Decisions: filingDecisionLimit}
	oldRevision, oldOK := budgetInteger(calls["revision"])
	newRevision, newOK := budgetInteger(revision)
	if !oldOK || !newOK {
		return exhausted
	}
	if oldRevision != newRevision {
		return filingBudget{}
	}
	if saved, exists := calls["budget"]; exists {
		b := obj(saved)
		if b == nil {
			return exhausted
		}
		e, okE := budgetInteger(b["evidence"])
		d, okD := budgetInteger(b["decisions"])
		p, okP := b["pending"].(bool)
		if !okE || !okD || !okP || e > filingEvidenceLimit || d > filingDecisionLimit || (p && d == 0) {
			return exhausted
		}
		return filingBudget{Evidence: e, Decisions: d, Pending: p}
	}
	return legacyFilingBudget(calls, newRevision)
}

// legacyFilingBudget rebuilds the budget of records saved before the budget was kept, from
// the check history of the same revision.
func legacyFilingBudget(calls Record, newRevision int) filingBudget {
	exhausted := filingBudget{Evidence: filingEvidenceLimit, Decisions: filingDecisionLimit}
	count, ok := budgetInteger(calls["count"])
	if !ok || calls["pending"] == true {
		return exhausted
	}
	checks := fleet.Elements(calls["checks"])
	current := make([]Record, 0, len(checks))
	for _, item := range checks {
		row := obj(item)
		if row == nil {
			return exhausted
		}
		rowRevision, valid := budgetInteger(row["revision"])
		if !valid {
			return exhausted
		}
		if rowRevision == newRevision {
			current = append(current, row)
		}
	}
	if len(current) != count {
		return exhausted
	}
	b := filingBudget{}
	for _, row := range current {
		switch fleet.Text(row["result"]) {
		case "evidence_requested":
			b.Evidence++
		case "allowed", "refused", "ai_failed", "invalid_answer":
			b.Decisions++
		default:
			return exhausted
		}
	}
	if b.Evidence > filingEvidenceLimit || b.Decisions > filingDecisionLimit {
		return exhausted
	}
	return b
}

// filingBudgetStopped reports whether the restored budget has reached the decision
// limit with known results. Evidence rounds alone never stop the check, and a
// reservation whose result is unknown is neither notified nor repaired.
func filingBudgetStopped(calls Record, revision any) (filingBudget, bool) {
	b := loadFilingBudget(calls, revision)
	return b, calls != nil && calls["pending"] != true && !b.Pending && b.Decisions >= filingDecisionLimit
}

func (b filingBudget) save(calls Record) {
	calls["budget"] = Record{"evidence": b.Evidence, "decisions": b.Decisions, "pending": b.Pending}
}

// resetFilingBudgetForRepair applies the same eligibility and consumption rules
// as the existing check path. The caller persists calls before another AI call.
func resetFilingBudgetForRepair(calls Record) bool {
	available := repairBaseline(calls)
	if _, stopped := filingBudgetStopped(calls, calls["revision"]); !available || !stopped {
		return false
	}
	consumeRepair(calls)
	filingBudget{}.save(calls)
	return true
}
func (b filingBudget) finalDecision() bool { return b.Evidence >= filingEvidenceLimit }

// The caller must save the reservation before calling the AI.
func (b *filingBudget) reserve() error {
	if b.Pending || b.Decisions >= filingDecisionLimit {
		return fmt.Errorf("filing decision budget unavailable")
	}
	b.Decisions++
	b.Pending = true
	return nil
}

// An evidence request during final decision remains a charged decision.
func (b *filingBudget) finish(result string) error {
	if !b.Pending {
		return fmt.Errorf("no filing reservation")
	}
	if result == "evidence_requested" && b.Evidence < filingEvidenceLimit {
		b.Decisions--
		b.Evidence++
	}
	b.Pending = false
	return nil
}
