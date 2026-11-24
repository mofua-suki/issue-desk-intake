package filing

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"strings"
	"testing"
)

type rejectedHistory struct {
	*memoryAPI
	reject bool
}

func (a *rejectedHistory) Issues(ctx context.Context, repo, state string) ([]map[string]any, error) {
	if a.reject {
		return nil, &gitea.Error{Status: 403}
	}
	return a.memoryAPI.Issues(ctx, repo, state)
}
func TestUnknownSubmissionRemainsUnknownAfterHistoryRead403(t *testing.T) {
	s, api := fixture(t)
	api.uncertain = true
	wrapped := &rejectedHistory{memoryAPI: api}
	s.options.API = wrapped
	first, e := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if e == nil || first["status"] != "submit_unknown" {
		t.Fatal(first, e)
	}
	wrapped.reject = true
	second, e := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if e == nil || second["status"] != "submit_unknown" || second["attempted"] != true {
		t.Fatalf("読み取り403で送信済み不明を消去: status=%v attempted=%v error=%v posts=%v", second["status"], second["attempted"], e, api.posts)
	}
}

type rejectedPost struct {
	*memoryAPI
	rejected bool
}

func (a *rejectedPost) CreateIssue(ctx context.Context, repo string, body any) (map[string]any, error) {
	if a.rejected {
		return nil, &gitea.Error{Status: 403}
	}
	return a.memoryAPI.CreateIssue(ctx, repo, body)
}
func TestRejectedCreationCanResumeWithoutUnknownOutcome(t *testing.T) {
	s, api := fixture(t)
	transport := &rejectedPost{memoryAPI: api, rejected: true}
	s.options.API = transport
	d, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err == nil || d["status"] != "submit_error" || d["attempted"] != false {
		t.Fatal(d, err)
	}
	transport.rejected = false
	d, err = s.Submit(context.Background(), draftID, Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" || api.posts != 1 {
		t.Fatal(d, err, api.posts)
	}
}
func TestAttachmentRejectionPreservesCreatedIssueAndDoesNotRepost(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
		d["messages"] = []any{Record{"images": []any{Record{"id": "fixture-photo"}}}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.options.Attach = func(context.Context, Record, map[string]any) error { return &gitea.Error{Status: 403} }
	d, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err == nil || d["status"] != "submit_unknown" || d["attempted"] != true || api.posts != 1 {
		t.Fatal(d, err, api.posts)
	}
	s.options.Attach = func(context.Context, Record, map[string]any) error { return nil }
	d, err = s.Submit(ctx, draftID, Record{}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" || api.posts != 1 {
		t.Fatal(d, err, api.posts)
	}
}
func TestSubmitFailureKeepsCauseOnTheDraft(t *testing.T) {
	s, api := fixture(t)
	s.options.API = &rejectedPost{memoryAPI: api, rejected: true}
	d, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
	if err == nil || d["status"] != "submit_error" {
		t.Fatal(d, err)
	}
	failure, _ := d["lastFailure"].(Record)
	if failure["stage"] != "起票" || fleet.Text(failure["cause"]) == "" || !strings.Contains(fleet.Text(d["error"]), fleet.Text(failure["cause"])) {
		t.Fatalf("起票の失敗の原因がありません: %+v", d)
	}
}

func TestUnpostedFilingResumesAfterTemporaryHistoryFailure(t *testing.T) {
	s, api := fixture(t)
	ctx := t.Context()
	p := auth.Principal{Kind: auth.Screen}
	api.historyErr = &gitea.Error{Status: 503}
	first, err := s.Submit(ctx, draftID, Record{"revision": 0}, p)
	if err == nil || first["status"] != "submitting" || first["attempted"] == true || api.posts != 0 {
		t.Fatalf("interrupted filing: draft=%v err=%v posts=%d", first, err, api.posts)
	}
	api.historyErr = nil
	for i := 0; i < 2; i++ {
		got, err := s.SubmitAccepted(ctx, draftID, Record{"revision": first["revision"]}, p)
		if err != nil || got["status"] != "submitted" || api.posts != 1 {
			t.Fatalf("recovered filing: draft=%v err=%v posts=%d", got, err, api.posts)
		}
	}
}

func TestSplitParentResumesAfterTemporaryChildHistoryFailure(t *testing.T) {
	s, api := fixture(t)
	ctx := t.Context()
	p := auth.Principal{Kind: auth.Screen}
	changeCorrectionRecord(t, s, draftID, func(d Record) { d["splitIssues"] = []any{Record{}, Record{}} })
	s.options.SplitPlan = func(Record) ([]Record, error) {
		return []Record{
			{"project": "owner/app", "title": "first", "body": "first", "changeScope": Record{"known": true, "paths": []any{"first.go"}}, "_dependsOn": []any{}},
			{"project": "owner/app", "title": "second", "body": "second", "changeScope": Record{"known": true, "paths": []any{"second.go"}}, "_dependsOn": []any{1}},
		}, nil
	}
	api.historyErr = &gitea.Error{Status: 503}
	first, err := s.Submit(ctx, draftID, Record{"revision": 0}, p)
	if err == nil || first["status"] != "submitting" || api.posts != 0 {
		t.Fatalf("interrupted split: draft=%v err=%v posts=%d", first, err, api.posts)
	}
	api.historyErr = nil
	got, err := s.SubmitAccepted(ctx, draftID, Record{"revision": first["revision"]}, p)
	if err != nil || got["status"] != "submitted" || len(fleet.Elements(got["issues"])) != 2 || api.posts != 2 {
		t.Fatalf("recovered split: draft=%v err=%v posts=%d", got, err, api.posts)
	}
}
