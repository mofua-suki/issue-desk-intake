package issueapi

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type failingHistory struct{ *fakeAPI }

func (f *failingHistory) Issues(context.Context, string, string) ([]Object, error) {
	return nil, fmt.Errorf("fixture history unavailable")
}
func TestCloseReasonBaselineKeepsClosedAtAndReopenBoundary(t *testing.T) {
	ctx := context.Background()
	db := testApplication(t)
	var e error
	api := &fakeAPI{issue: Object{"number": float64(1), "state": "closed", "closed_at": "2026-10-01T00:00:00Z", "user": Object{"id": float64(1), "login": "intake"}}}
	s := &Service{o: Options{AppDB: db.DB(), Gitea: api, Now: time.Now, Repositories: func() []string { return []string{"owner/repo"} }, Own: func(u Object) bool { return u["id"] == float64(1) }, Config: Object{}}}
	before, e := s.closeReason(ctx, "owner/repo", api.issue)
	if e != nil || before["source"] != "unconfirmed" {
		t.Fatalf("before baseline %v %v", before, e)
	}
	s.o.Gitea = &failingHistory{api}
	if e = s.ensureCloseBaseline(ctx); e == nil {
		t.Fatal("incomplete history accepted")
	}
	if baseline, _ := s.closeBaseline(ctx); baseline != nil {
		t.Fatal("incomplete history fixed an empty baseline")
	}
	s.o.Gitea = api
	if e = s.ensureCloseBaseline(ctx); e != nil {
		t.Fatal(e)
	}
	legacy, e := s.closeReason(ctx, "owner/repo", api.issue)
	if e != nil || legacy["reason"] != "other_route" || legacy["source"] != "legacy" {
		t.Fatalf("legacy %v %v", legacy, e)
	}
	_ = s.save(ctx, "close_reasons", "owner/repo#1", Object{"reason": "duplicate", "source": "token", "closedAt": api.issue["closed_at"]})
	token, e := s.closeReason(ctx, "owner/repo", api.issue)
	if e != nil || token["reason"] != "duplicate" || token["source"] != "token" {
		t.Fatalf("manual override %v %v", token, e)
	}
	if e = s.forgetCloseReason(ctx, "owner/repo", 1); e != nil {
		t.Fatal(e)
	}
	api.issue["closed_at"] = "2026-11-01T00:00:00Z"
	again, e := s.closeReason(ctx, "owner/repo", api.issue)
	if e != nil || again["reason"] != "unknown" || again["source"] != "none" {
		t.Fatalf("stale close reused %v %v", again, e)
	}
	if e = s.ensureCloseBaseline(ctx); e != nil {
		t.Fatal(e)
	}
	again, _ = s.closeReason(ctx, "owner/repo", api.issue)
	if again["reason"] != "unknown" {
		t.Fatal("baseline expanded after it was fixed")
	}
}
