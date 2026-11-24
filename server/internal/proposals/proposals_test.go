package proposals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/pgtest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/store"
)

type fakeAPI struct {
	comment Object
	before  func()
}

func (f *fakeAPI) Do(_ context.Context, method, path string, _ any, out any) error {
	if f.before != nil {
		f.before()
	}
	if method != "GET" || path != "/repos/owner/repo/issues/comments/10" {
		return fmt.Errorf("unexpected fixture operation")
	}
	raw, _ := json.Marshal(f.comment)
	return json.Unmarshal(raw, out)
}
func fixture(t *testing.T) (*Service, *fakeAPI, func()) {
	t.Helper()
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	db := openStore(t, env)
	t.Cleanup(func() { db.Close() })
	records, e := consultations.OpenRecords(ctx, db.DB())
	if e != nil {
		t.Fatal(e)
	}
	p := Object{"version": 1, "kind": "followup", "id": "followup-1", "source": "owner/repo#1", "title": "後続の案", "purpose": "説明<&>😀", "acceptance": []any{"動く"}, "dependsOn": []any{}, "provider": "codex"}
	raw, _ := json.Marshal(p)
	api := &fakeAPI{comment: Object{"id": 10, "body": "<!-- issue-desk-proposal " + string(raw) + " -->", "issue_url": "https://git.example/api/v1/repos/owner/repo/issues/1", "user": Object{"login": "old"}, "created_at": "2026-11-01T00:00:00.000Z", "updated_at": "2026-11-01T00:05:00.000Z"}}
	s := &Service{o: Options{AppDB: db.DB(), Gitea: api, Now: func() time.Time { return time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC) }, Project: func(repo string) error {
		if repo != "owner/repo" {
			return fail(400, "unknown")
		}
		return nil
	}, Config: func() (Object, error) {
		return Object{"gitUser": "old", "intake": Object{"proposalWriters": []any{"worker"}}, "assignments": Object{"repositories": Object{"owner/repo": "host-a"}, "implementers": Object{"owner/repo": []any{"host-a", "host-b"}}}}, nil
	}}, records: records}
	rejection := Object{"type": "rejection", "repo": "owner/repo", "number": 1, "commentId": 10, "id": "followup-1", "author": "old", "status": "rejected", "postedAt": "2026-11-01T00:00:00.000Z", "checkedAt": "2026-11-01T00:10:00.000Z", "extra": "keep"}
	if e = s.save(ctx, "rejection:owner/repo#1:10", rejection); e != nil {
		t.Fatal(e)
	}
	restart := func() {
		db.Close()
		db = openStore(t, env)
		records, e = consultations.OpenRecords(ctx, db.DB())
		if e != nil {
			t.Fatal(e)
		}
		s.o.AppDB = db.DB()
		s.records = records
	}
	return s, api, restart
}
func data() Object {
	return Object{"project": "owner/repo", "number": float64(1), "commentId": float64(10), "proposalId": "followup-1"}
}
func status(e error) int {
	if p, ok := e.(*contract.Problem); ok {
		return p.Status
	}
	return 0
}
func TestProposalFingerprintAndValidation(t *testing.T) {
	s, api, _ := fixture(t)
	_, hash, e := parse(str(api.comment["body"]))
	if e != nil || hash != "33cd241b04e8822a47964faed6ac1f36e6e1ad9a86ec07cf71ea71216db9c825" {
		t.Fatalf("%s %v", hash, e)
	}
	if draftID("owner/repo", 1, "followup-1") != "0134ed79-d403-57fc-9e6a-0e30704f1bbb" {
		t.Fatal("draft identity changed")
	}
	p := Object{"version": 3, "kind": "followup", "id": "followup-1", "source": "owner/repo#1", "title": "後続の案", "pr": "https://git.example/owner/repo/pulls/2", "items": []any{Object{"problem": "不具合", "expected": "正常", "test": "検証"}}, "dependsOn": []any{}, "provider": "codex", "changeScope": nil}
	raw, _ := json.Marshal(p)
	_, hash, e = parse("<!-- issue-desk-proposal " + string(raw) + " -->")
	if e != nil || hash != "c2b6b2112c04e77840e57f8c7a8fd7714f6888a0011ca1a2a0965f7494f610b2" {
		t.Fatalf("%s %v", hash, e)
	}
	for _, bad := range []string{str(api.comment["body"]) + str(api.comment["body"]), strings.Replace(str(api.comment["body"]), `"codex"`, `"unknown"`, 1), strings.Replace(str(api.comment["body"]), `"version":1`, `"version":1.5`, 1)} {
		if _, _, e := parse(bad); status(e) != 409 {
			t.Fatalf("accepted invalid proposal: %v", e)
		}
	}
	if _, e := s.Candidate(context.Background(), Object{"project": "owner/repo", "number": 1.5, "commentId": 10, "proposalId": "followup-1"}); status(e) != 400 {
		t.Fatal("fractional issue accepted")
	}
}
func TestDismissalPersistsExactFingerprint(t *testing.T) {
	ctx := context.Background()
	s, api, restart := fixture(t)
	initial, e := s.UnfiledProposalAlerts(ctx)
	if e != nil || len(initial) != 1 {
		t.Fatalf("rejected alert: %v %v", initial, e)
	}
	input := data()
	candidate, e := s.Candidate(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	input["fingerprint"] = candidate["fingerprint"]
	input["reason"] = "不要"
	input["id"] = "00000000-0000-4000-8000-000000000001"
	input["instructionAt"] = "2026-11-01T00:04:00Z"
	if _, e = s.Dismiss(ctx, input, "token:pc", true); status(e) != 409 {
		t.Fatalf("older instruction: %v", e)
	}
	input["instructionAt"] = "2026-11-01T00:05:00Z"
	mark, e := s.Dismiss(ctx, input, "token:pc", true)
	if e != nil {
		t.Fatal(e)
	}
	restart()
	same, e := s.Dismiss(ctx, input, "token:pc", true)
	if e != nil || same["operationId"] != mark["operationId"] {
		t.Fatalf("resend %v", e)
	}
	input["reason"] = "変更"
	if _, e = s.Dismiss(ctx, input, "screen", false); status(e) != 409 {
		t.Fatalf("reused ID %v", e)
	}
	alerts, e := s.UnfiledProposalAlerts(ctx)
	if e != nil || len(alerts) != 0 {
		t.Fatalf("dismissed alerts %v %v", alerts, e)
	}
	restart()
	alerts, e = s.UnfiledProposalAlerts(ctx)
	if e != nil || len(alerts) != 0 {
		t.Fatalf("dismissal after restart %v %v", alerts, e)
	}
	api.comment["user"] = Object{"login": "other"}
	if _, e = s.Candidate(ctx, data()); status(e) != 409 {
		t.Fatal("source author mismatch accepted")
	}
}
func TestDismissalRejectsConcurrentAdoptionAndChangedContent(t *testing.T) {
	s, api, _ := fixture(t)
	ctx := context.Background()
	input := data()
	candidate, e := s.Candidate(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	input["fingerprint"] = candidate["fingerprint"]
	input["id"] = "00000000-0000-4000-8000-000000000001"
	input["reason"] = "不要"
	api.comment["body"] = strings.Replace(str(api.comment["body"]), "後続の案", "変更後", 1)
	if _, e = s.Dismiss(ctx, input, "screen", false); status(e) != 409 {
		t.Fatalf("changed content %v", e)
	}
	api.comment["body"] = strings.Replace(str(api.comment["body"]), "変更後", "後続の案", 1)
	api.before = func() {
		api.before = nil
		_, e := s.records.Change(ctx, "drafts", draftID("owner/repo", 1, "followup-1"), func(*sql.Tx, consultations.Record) (consultations.Record, error) {
			return consultations.Record{"id": "adopted"}, nil
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Dismiss(ctx, input, "screen", false); status(e) != 409 {
		t.Fatalf("concurrent adoption %v", e)
	}
	if mark, e := s.read(ctx, "dismissal:owner/repo#1:followup-1"); e != nil || mark != nil {
		t.Fatal("adopted proposal overwritten")
	}
}
func TestUnfiledAlertsAgeCompletionAndMissingStorage(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	r, _ := s.read(ctx, "rejection:owner/repo#1:10")
	r["postedAt"] = s.now().Add(-time.Hour).Format(time.RFC3339Nano)
	_ = s.save(ctx, "rejection:owner/repo#1:10", r)
	if alerts, e := s.UnfiledProposalAlerts(ctx); e != nil || len(alerts) != 0 {
		t.Fatal("one-hour boundary changed")
	}
	r["postedAt"] = s.now().Add(-7*24*time.Hour - time.Millisecond).Format(time.RFC3339Nano)
	_ = s.save(ctx, "rejection:owner/repo#1:10", r)
	if alerts, e := s.UnfiledProposalAlerts(ctx); e != nil || len(alerts) != 0 {
		t.Fatal("old observation accepted")
	}
	r["postedAt"] = s.now().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	_ = s.save(ctx, "rejection:owner/repo#1:10", r)
	_ = s.save(ctx, "owner/repo#1:followup-1", Object{"type": "proposal", "repo": "owner/repo", "number": 1, "id": "followup-1", "status": "notified"})
	if alerts, e := s.UnfiledProposalAlerts(ctx); e != nil || len(alerts) != 0 {
		t.Fatal("completed proposal alerted")
	}
	s.o.AppDB.Close()
	if _, e := s.UnfiledProposalAlerts(ctx); e == nil {
		t.Fatal("failed read became empty success")
	}
}
func TestProposalRoutesRequireAuthentication(t *testing.T) {
	s, _, _ := fixture(t)
	// A request without credentials must fail before reading the proposal record.
	request := httptest.NewRequest(http.MethodGet, "https://desk.example/api/proposals/dismissal", nil)
	// Nil store is safe for requests without credentials.
	a, e := auth.New(auth.Config{PublicURL: "https://desk.example", Port: 8787}, &store.AuthStore{})
	if e != nil {
		t.Fatal(e)
	}
	s.o.Auth = a
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatalf("status %d", response.Code)
	}
}

func TestScanExcludedDismissalSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, _, restart := fixture(t)
	rejection, err := s.read(ctx, "rejection:owner/repo#1:10")
	if err != nil {
		t.Fatal(err)
	}
	rejection["status"] = "scan_excluded"
	if err := s.save(ctx, "rejection:owner/repo#1:10", rejection); err != nil {
		t.Fatal(err)
	}
	alerts, err := s.UnfiledProposalAlerts(ctx)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("scan-excluded alert: %v %v", alerts, err)
	}
	input := data()
	candidate, err := s.Candidate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input["fingerprint"] = candidate["fingerprint"]
	input["reason"] = "不要"
	input["id"] = "00000000-0000-4000-8000-000000000001"
	if _, err = s.Dismiss(ctx, input, "screen", false); err != nil {
		t.Fatal(err)
	}
	restart()
	alerts, err = s.UnfiledProposalAlerts(ctx)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("scan-excluded dismissal after restart: %v %v", alerts, err)
	}
}
