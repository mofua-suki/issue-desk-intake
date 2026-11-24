package filingcheck

import (
	"encoding/json"
	"testing"
)

func TestFilingBudgetTransitions(t *testing.T) {
	b := filingBudget{}
	for i := 0; i < 8; i++ {
		if err := b.reserve(); err != nil {
			t.Fatal(err)
		}
		if err := b.finish("evidence_requested"); err != nil {
			t.Fatal(err)
		}
		if i == 3 && b.Decisions != 0 {
			t.Fatalf("four evidence rounds consumed decisions: %+v", b)
		}
	}
	if !b.finalDecision() {
		t.Fatal("eighth evidence round must require final decision")
	}
	for i := 0; i < 5; i++ {
		if err := b.reserve(); err != nil {
			t.Fatal(err)
		}
		if err := b.finish("evidence_requested"); err != nil {
			t.Fatal(err)
		}
	}
	if b.Evidence != 8 || b.Decisions != 5 {
		t.Fatalf("limits: %+v", b)
	}
	if b.reserve() == nil {
		t.Fatal("sixth decision reservation accepted")
	}
}

func TestFilingBudgetReservations(t *testing.T) {
	for _, result := range []string{"ai_failed", "unknown"} {
		t.Run(result, func(t *testing.T) {
			calls := Record{"revision": 1, "count": 0}
			b := loadFilingBudget(calls, 1)
			if err := b.reserve(); err != nil {
				t.Fatal(err)
			}
			b.save(calls)
			data, err := json.Marshal(calls)
			if err != nil {
				t.Fatal(err)
			}
			var restored Record
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			b = loadFilingBudget(restored, 1)
			if !b.Pending || b.Decisions != 1 || b.reserve() == nil {
				t.Fatalf("lost pending reservation: %+v", b)
			}
			if result != "unknown" {
				if err := b.finish(result); err != nil {
					t.Fatal(err)
				}
			}
			b.save(restored)
			data, err = json.Marshal(restored)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if got := loadFilingBudget(restored, 1); got.Decisions != 1 || got.Pending != (result == "unknown") {
				t.Fatalf("reservation state after reload: %+v", got)
			}
		})
	}
}

func TestFilingBudgetLegacyAndRepair(t *testing.T) {
	row := func(result string) any { return Record{"revision": 3, "result": result} }
	complete := Record{"revision": 3, "count": 2, "checks": []any{row("evidence_requested"), row("refused")}}
	if b := loadFilingBudget(complete, 3); b.Evidence != 1 || b.Decisions != 1 {
		t.Fatalf("complete history: %+v", b)
	}
	cases := []Record{
		{"revision": 3, "count": 2, "checks": []any{row("refused")}},
		{"revision": 3, "count": 21, "checks": make([]any, 20)},
		{"revision": 3, "count": 1, "checks": []any{row("")}},
		{"revision": 3, "count": 1, "pending": true, "checks": []any{row("refused")}},
	}
	for _, calls := range cases {
		b := loadFilingBudget(calls, 3)
		if b.Evidence != 8 || b.Decisions != 5 {
			t.Fatalf("incomplete history granted budget: %+v", b)
		}
	}
	if b := loadFilingBudget(complete, 4); b.Evidence != 0 || b.Decisions != 0 {
		t.Fatalf("new revision: %+v", b)
	}
	complete["count"] = 5
	complete["outcome"] = "refused"
	complete["recovery"] = Record{"baseline": Record{"refused": "0"}, "used": Record{"refused": []any{"0"}}}
	if !resetFilingBudgetForRepair(complete) {
		t.Fatal("eligible repair was not reset")
	}
	if b := loadFilingBudget(complete, 3); b.Evidence != 0 || b.Decisions != 0 {
		t.Fatalf("repair: %+v", b)
	}
	if resetFilingBudgetForRepair(complete) {
		t.Fatal("repair was reused")
	}
}
