package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

func TestWaitBreakdownPrioritizesDependencyScopeAndSlots(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ev := func(id int64, second int, kind, host, to string) event {
		return event{Job: id, At: at.Add(time.Duration(second) * time.Second), Kind: kind, Host: host, Data: map[string]any{"to": to}}
	}
	target := &waitJob{ID: 1, Repo: "o/r", Filed: at, Scope: []string{"src/"}, Deps: []int64{2}, Events: []event{ev(1, 8, "leased", "nas", "")}}
	dep := &waitJob{ID: 2, Repo: "o/r", Filed: at, Scope: []string{"other/"}, Events: []event{ev(2, 0, "leased", "other", ""), ev(2, 2, "state", "other", "merged")}}
	overlap := &waitJob{ID: 3, Repo: "o/r", Filed: at, Scope: []string{"src/x.go"}, Events: []event{ev(3, 2, "leased", "other", ""), ev(3, 4, "state", "other", "merged")}}
	busy := &waitJob{ID: 4, Repo: "o/other", Filed: at, Scope: []string{}, Events: []event{ev(4, 4, "leased", "nas", ""), ev(4, 6, "state", "nas", "merged")}}
	all := map[int64]*waitJob{1: target, 2: dep, 3: overlap, 4: busy}
	a := &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: 1}}}
	starts, _, err := waitEntries(context.Background(), all, Period{at, at.Add(9 * time.Second)}, "o/r", a, newWaitScopeCache(a))
	if err != nil {
		t.Fatal(err)
	}
	var mine waitEntry
	for _, e := range starts {
		if e.Duration != nil && *e.Duration == 8000 {
			mine = e
		}
	}
	for _, cause := range []string{"dependency", "scopeOverlap", "slotFull", "idle"} {
		if mine.Parts[cause] != 2000 {
			t.Fatalf("%s: %+v", cause, mine)
		}
	}
	if mine.Missing {
		t.Fatal(mine)
	}
	a.Hosts["nas"].Max = 0
	starts, _, err = waitEntries(context.Background(), map[int64]*waitJob{1: target}, Period{at, at.Add(9 * time.Second)}, "o/r", a, newWaitScopeCache(a))
	if err != nil {
		t.Fatal(err)
	}
	summary := waitSummary(starts, []string{"slotFull", "unknown"})
	if summary["causes"].(map[string]any)["slotFull"].(map[string]any)["ms"] != nil {
		t.Fatal(summary)
	}
}
