package ai

import (
	"context"
	"testing"
)

func TestSessionCostDoesNotTurnMissingOrDecreasedCostIntoZero(t *testing.T) {
	total, base := 0.2, 0.3
	u := SessionCost(Usage{CostUSD: &total}, &base, true, false)
	if u.CostUSD != nil || u.CostUnknownReason == nil || *u.CostUnknownReason != "total-decreased" {
		t.Fatal(u)
	}
	zero := 0.0
	u = SessionCost(Usage{CostUSD: &zero}, nil, false, false)
	if u.CostUSD == nil || *u.CostUSD != 0 {
		t.Fatal("実測0が欠測になった")
	}
}
func TestLiveCallsTrackOnlyActiveRequestMetadata(t *testing.T) {
	r := &Runner{}
	end := r.live(Request{Draft: "d", Input: "秘密", Provider: "codex", Step: "工程"})
	calls, err := r.Calls(context.Background())
	if err != nil || len(calls) != 1 || calls[0].Draft != "d" {
		t.Fatal(calls, err)
	}
	end()
	calls, err = r.Calls(context.Background())
	if err != nil || len(calls) != 0 {
		t.Fatal(calls, err)
	}
}
