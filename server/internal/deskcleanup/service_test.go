package deskcleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/pgtest"
	"maps"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	s          *Service
	row        Object
	busy       map[string]bool
	states     map[int64]string
	closed     int
	history    []Object
	historyErr error
}

func setup(t *testing.T) *fixture {
	local := openStore(t, pgtest.NewIsolated(t))
	t.Cleanup(func() { local.Close() })
	f := &fixture{busy: map[string]bool{}, states: map[int64]string{1: "open"}, row: Object{"key": "case", "id": "stall-case", "reason": "原因A", "updatedAt": "old"}}
	f.s = &Service{DB: local.DB(), Project: func(repo string) error {
		if repo != "owner/repo" {
			return fail(400, "対象外")
		}
		return nil
	}, Busy: func(id string) bool { return f.busy[id] }, Own: func(u Object) bool { return u["id"] == float64(10) }, Issue: func(_ context.Context, repo string, n int64) (Object, error) {
		return Object{"title": "対象", "state": f.states[n], "status": "failed", "updatedAt": "fixed"}, nil
	}, History: func(context.Context, string) ([]Object, error) { return f.history, f.historyErr }, Alerts: func(context.Context, string) ([]any, error) { return []any{f.row}, nil }, CloseIssue: func(_ context.Context, _ string, n int64, _, _ string, _ auth.Principal) (any, error) {
		f.closed++
		f.states[n] = "closed"
		return Object{"state": "closed"}, nil
	}}
	return f
}
func (f *fixture) put(t *testing.T, id string, d Object) {
	t.Helper()
	d["id"] = id
	raw, _ := json.Marshal(d)
	if _, e := f.s.DB.Exec(`INSERT INTO drafts(id,json) VALUES(?,?)ON CONFLICT(id)DO UPDATE SET json=excluded.json`, id, string(raw)); e != nil {
		t.Fatal(e)
	}
}
func (f *fixture) request(t *testing.T, target string) Object {
	t.Helper()
	candidate, e := f.s.Inspect(t.Context(), target)
	if e != nil {
		t.Fatal(e)
	}
	return Object{"target": target, "fingerprint": candidate["fingerprint"], "reason": "利用者の明示した終了", "userStatement": "この対象の終了をお願いします。"}
}
func TestExplicitDraftEndingChecksFamilyAndIssueHistory(t *testing.T) {
	f := setup(t)
	f.put(t, "parent", Object{"project": "owner/repo", "status": "submit_unknown", "revision": 1, "attempted": true, "consultationChildren": []any{"child"}})
	f.put(t, "child", Object{"status": "draft"})
	f.history = []Object{{"number": 1, "body": "<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->", "user": Object{"id": float64(10)}}}
	data := f.request(t, "draft:parent")
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("open child accepted")
	}
	f.put(t, "child", Object{"status": "closed"})
	f.busy["child"] = true
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("busy child accepted")
	}
	f.busy["child"] = false
	f.historyErr = &gitea.Error{Status: 403}
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("unreadable history treated as empty")
	}
	f.historyErr = nil
	// An authenticated marker for the actual draft is included even without a
	// saved issue reference. No CreateIssue callback exists on this service.
	id := "11111111-1111-4111-8111-111111111111"
	f.put(t, id, Object{"project": "owner/repo", "status": "submit_unknown", "attempted": true})
	unknown := f.request(t, "draft:"+id)
	if _, e := f.s.Apply(t.Context(), unknown, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("open marker Issue ignored")
	}
	f.states[1] = "closed"
	if _, e := f.s.Apply(t.Context(), unknown, auth.Principal{Kind: auth.Screen}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e != nil {
		t.Fatal("replay failed", e)
	}
	records, e := Archived(t.Context(), f.s.DB)
	if e != nil || len(records) != 2 {
		t.Fatal("archive duplicated", len(records), e)
	}
	d, e := draft(t.Context(), f.s.DB, "parent")
	if e != nil || d["status"] != "closed" || d["cleanup"] == nil || d["attempted"] != true {
		t.Fatal(d, e)
	}
}
func TestSnapshotMismatchAndAlertReappearance(t *testing.T) {
	f := setup(t)
	f.put(t, "d", Object{"project": "owner/repo", "status": "draft", "revision": 1})
	data := f.request(t, "draft:d")
	f.put(t, "d", Object{"project": "owner/repo", "status": "draft", "revision": 2})
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("stale snapshot accepted")
	}
	for _, kind := range []string{"attention", "stall"} {
		target := kind + ":" + AlertID(kind, f.row)
		data = f.request(t, target)
		if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.PC, Name: "nas"}); e != nil {
			t.Fatal(e)
		}
		f.row["updatedAt"] = "new"
		rows, e := VisibleAlerts(t.Context(), f.s.DB, kind, []any{f.row})
		if e != nil || len(rows) != 0 {
			t.Fatal("timestamp reappeared", rows, e)
		}
		f.row["reason"] = "原因B"
		rows, e = VisibleAlerts(t.Context(), f.s.DB, kind, []any{f.row})
		if e != nil || len(rows) != 1 {
			t.Fatal("changed reason hidden", rows, e)
		}
		f.row["reason"] = "原因A"
	}
	if f.row["reason"] != "原因A" {
		t.Fatal("source mutated")
	}
}
func TestIssueEndingReplayRejectsReopen(t *testing.T) {
	f := setup(t)
	data := f.request(t, "issue:owner/repo#1")
	p := auth.Principal{Kind: auth.PC, Name: "nas", Delegated: true}
	if _, e := f.s.Apply(t.Context(), data, p); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Apply(t.Context(), data, p); e != nil || f.closed != 1 {
		t.Fatal("close repeated", e)
	}
	f.states[1] = "open"
	if _, e := f.s.Apply(t.Context(), data, p); e == nil {
		t.Fatal("reopened issue ended from stale receipt")
	}
}

type credentials struct{}

func (credentials) PCToken(_ context.Context, h string) (auth.Token, bool, error) {
	return auth.Token{ID: "fixture", Name: "nas", Delegated: h == auth.Hash("idk_"+strings.Repeat("d", 43))}, h == auth.Hash("idk_"+strings.Repeat("d", 43)) || h == auth.Hash("idk_"+strings.Repeat("p", 43)), nil
}
func (credentials) TouchPCToken(context.Context, string, time.Time) error { return nil }
func (credentials) HostdToken(context.Context, string) (auth.Token, bool, error) {
	return auth.Token{Name: "nas"}, true, nil
}
func (credentials) Session(context.Context, string) (auth.Session, bool, error) {
	return auth.Session{CSRF: "fixture", ExpiresAt: time.Now().Add(time.Hour)}, true, nil
}
func (credentials) ConsumeCLIKey(context.Context, string, time.Time) (bool, error) { return true, nil }
func TestHTTPAuthorizationAndNoReadSideEffects(t *testing.T) {
	f := setup(t)
	a, e := auth.New(auth.Config{PublicURL: "https://desk.test", Port: 8787}, credentials{})
	if e != nil {
		t.Fatal(e)
	}
	f.s.Auth = a
	for _, entry := range []struct {
		kind, value string
		want        int
	}{{"Authorization", "Bearer idk_" + strings.Repeat("p", 43), 403}, {"Authorization", "Bearer idh_" + strings.Repeat("a", 43), 403}, {"X-Desk-Key", "fixture", 403}, {"Authorization", "Bearer idk_" + strings.Repeat("d", 43), 200}} {
		r := httptest.NewRequest("POST", "https://desk.test/api/desk-cleanup/inspect", strings.NewReader(`{"target":"stall:stall-case"}`))
		if entry.kind == "X-Desk-Key" {
			r = httptest.NewRequest("POST", "http://127.0.0.1:8787/api/desk-cleanup/inspect", strings.NewReader(`{"target":"stall:stall-case"}`))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Origin", "http://127.0.0.1:8787")
		} else {
			r.Header.Set("Origin", "https://desk.test")
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(entry.kind, entry.value)
		w := httptest.NewRecorder()
		f.s.ServeHTTP(w, r)
		if w.Code != entry.want {
			t.Fatalf("%s HTTP%d want%d body=%s", entry.kind, w.Code, entry.want, w.Body.String())
		}
	}
	records, e := Archived(t.Context(), f.s.DB)
	if e != nil || len(records) != 0 {
		t.Fatal("inspect changed state")
	}

}

func TestDraftChangeDuringRelatedIssueReadIsRejected(t *testing.T) {
	f := setup(t)
	f.put(t, "d", Object{"project": "owner/repo", "status": "draft", "revision": 1, "issue": Object{"number": 1}})
	data := f.request(t, "draft:d")
	f.states[1] = "closed"
	original := f.s.Issue
	f.s.Issue = func(ctx context.Context, repo string, n int64) (Object, error) {
		f.put(t, "d", Object{"project": "owner/repo", "status": "draft", "revision": 2, "issue": Object{"number": 1}})
		return original(ctx, repo, n)
	}
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("await中の変更を見落とした")
	}
	d, e := draft(t.Context(), f.s.DB, "d")
	if e != nil || d["status"] == "closed" {
		t.Fatal(d, e)
	}
}

func TestArchiveCompatibilityAndMalformedReferences(t *testing.T) {
	f := setup(t)
	old := Object{"target": "stall:stall-case", "fingerprint": "legacy-ordered-json-hash", "snapshot": f.row}
	raw, _ := json.Marshal([]Object{old})
	if _, e := f.s.DB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, string(raw)); e != nil {
		t.Fatal(e)
	}
	rows, e := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{f.row})
	if e != nil || len(rows) != 0 {
		t.Fatal("既存snapshotの記録を保持しない", rows, e)
	}
	f.put(t, "bad", Object{"status": "draft", "project": "owner/repo", "issue": Object{"number": 1.5}})
	data := f.request(t, "draft:bad")
	if _, e := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); e == nil {
		t.Fatal("不正な関連番号を切り捨てた")
	}
}

func TestSystemProjectAlertsCanBeAcknowledgedWithoutConfiguredRepository(t *testing.T) {
	for _, project := range []string{fleet.SystemProject, fleet.LegacySystemProject} {
		for _, prior := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prior=%v", project, prior), func(t *testing.T) {
				f := setup(t)
				f.row["project"] = project
				if prior {
					old := Object{"id": "stall-case", "project": fleet.LegacySystemProject, "reason": "old cause"}
					raw, _ := json.Marshal([]Object{{"target": "stall:stall-case", "fingerprint": AlertFingerprint(old), "snapshot": old}})
					if _, err := f.s.DB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
						t.Fatal(err)
					}
				}
				data := f.request(t, "stall:stall-case")
				if _, err := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); err != nil {
					t.Fatal(err)
				}
				visible, err := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{f.row})
				if err != nil || len(visible) != 0 {
					t.Fatalf("visible=%v error=%v", visible, err)
				}
			})
		}
	}
	f := setup(t)
	f.row["project"] = "unconfigured/repo"
	if _, err := f.s.Inspect(t.Context(), "stall:stall-case"); err == nil {
		t.Fatal("unconfigured project accepted")
	}
}

func TestElapsedAndAttentionTargetsCanBeClosed(t *testing.T) {
	for _, target := range []string{"attention:rc-update:nas", "attention:attention:rc-update:nas"} {
		t.Run(target, func(t *testing.T) {
			f := setup(t)
			f.row = Object{"key": "rc-update:nas", "kind": "rc_update_missing", "project": fleet.SystemProject, "reason": "missing"}
			data := f.request(t, target)
			candidate, err := f.s.Inspect(t.Context(), target)
			if err != nil || candidate["target"] != "attention:attention:rc-update:nas" {
				t.Fatal(candidate, err)
			}
			result, err := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen})
			if err != nil || result["target"] != candidate["target"] {
				t.Fatal(result, err)
			}
			for _, alias := range []string{"attention:rc-update:nas", "attention:attention:rc-update:nas"} {
				data["target"] = alias
				if _, err := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); err != nil {
					t.Fatal(err)
				}
			}
			visible, err := VisibleAlerts(t.Context(), f.s.DB, "attention", []any{f.row})
			if err != nil || len(visible) != 0 {
				t.Fatal(visible, err)
			}
		})
	}
	for _, kind := range []string{"urgent_fix_hold", "job_dispatch_stall"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			before, after := "状態: 保留 / 30分停止", "状態: 保留 / 31分停止"
			if kind == "job_dispatch_stall" {
				before, after = "実行待ち: 60分\n直近の見送り: 理由 / 取得PC: nas / 時刻（10分前）", "実行待ち: 61分\n直近の見送り: 理由 / 取得PC: nas / 時刻（11分前）"
			}
			f.row = Object{"id": "one", "kind": kind, "detail": before}
			data := f.request(t, "stall:one")
			f.row["detail"] = after
			if _, err := f.s.Apply(t.Context(), data, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			visible, err := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{f.row})
			if err != nil || len(visible) != 0 {
				t.Fatal(visible, err)
			}
		})
	}
}

func TestArchivedIssueMovesOnlyTheUnchangedListEntry(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			f := setup(t)
			progress := Object{"pending": 1, "checks": []any{Object{"status": "failed"}}}
			issue := Object{"project": "owner/repo", "number": 1, "desk": 6355, "title": "対象",
				"state": "closed", "status": status, "updatedAt": "fixed", "executionProgress": progress,
				"result": Object{"status": "failed", "detail": "失敗結果"}}
			original, _ := json.Marshal(issue)
			f.s.Issue = func(context.Context, string, int64) (Object, error) { return issue, nil }
			f.s.CloseIssue = func(context.Context, string, int64, string, string, auth.Principal) (any, error) {
				return Object{"state": "closed", "alreadyClosed": true}, nil
			}
			if _, e := f.s.Apply(t.Context(), f.request(t, "issue:owner/repo#1"), auth.Principal{Kind: auth.Screen}); e != nil {
				t.Fatal(e)
			}
			before, e := Archived(t.Context(), f.s.DB)
			if e != nil {
				t.Fatal(e)
			}
			stale := maps.Clone(issue)
			stale["updatedAt"] = "changed"
			reopened := maps.Clone(issue)
			reopened["state"] = "open"
			unrecorded := maps.Clone(issue)
			unrecorded["number"] = 2
			rows := []Object{issue, stale, reopened, unrecorded}
			if e := ApplyIssueArchives(t.Context(), f.s.DB, rows); e != nil {
				t.Fatal(e)
			}
			if rows[0]["status"] != "closed" || rows[1]["status"] != status || rows[2]["status"] != status || rows[3]["status"] != status {
				t.Fatal(rows)
			}
			expected := maps.Clone(issue)
			expected["status"] = "closed"
			if !reflect.DeepEqual(rows[0], expected) {
				t.Fatal(rows[0], expected)
			}
			afterIssue, _ := json.Marshal(issue)
			after, e := Archived(t.Context(), f.s.DB)
			if e != nil || string(original) != string(afterIssue) || !reflect.DeepEqual(before, after) {
				t.Fatal("source or archive changed", issue, before, after, e)
			}
		})
	}
}
