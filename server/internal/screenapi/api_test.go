package screenapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureID = "11111111-1111-4111-8111-111111111111"
const otherID = "22222222-2222-4222-8222-222222222222"

type fixture struct {
	t           *testing.T
	db          *store.SQLite
	s           *Service
	mux         *http.ServeMux
	options     Options
	now         time.Time
	token       string
	sourceCalls atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := openStore(t, pgtest.NewIsolated(t))
	t.Cleanup(func() { db.Close() })
	var e error
	f := &fixture{t: t, db: db, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), token: "idk_" + strings.Repeat("a", 43)}
	if e = db.CreateSession(ctx, auth.Hash("screen"), auth.Session{CSRF: "csrf", ExpiresAt: f.now.Add(time.Hour)}, f.now); e != nil {
		t.Fatal(e)
	}
	if _, e = db.AddPCToken(ctx, store.PCTokenRecord{ID: fixtureID, Name: "worker", Hash: auth.Hash(f.token), IssuedAt: f.now}); e != nil {
		t.Fatal(e)
	}
	if _, e = db.SetDelegated(ctx, fixtureID, true, f.now); e != nil {
		t.Fatal(e)
	}
	a, e := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787", Now: func() time.Time { return f.now }}, &store.AuthStore{SQLite: db})
	if e != nil {
		t.Fatal(e)
	}
	f.options = Options{Auth: a, AppDB: db.DB(), RuntimeDir: t.TempDir(), RepoRoot: "/repo", Now: func() time.Time { return f.now }, Config: func() (Object, error) { return Object{"repositories": []any{Object{"name": "owner/repo"}}}, nil }, Lists: map[string]ListReader{}, Entries: map[string]EntryReader{}, Aux: map[string]func(context.Context) (any, error){}, Meta: func(context.Context) (Object, error) { return Object{"worker": true}, nil }}
	for _, kind := range rowTypes {
		kind := kind
		f.options.Lists[kind] = func(context.Context, string) ([]Object, error) {
			f.sourceCalls.Add(1)
			if kind == "draft" {
				return []Object{{"id": fixtureID, "project": "owner/repo", "title": "相談", "messages": []any{Object{"role": "user", "text": strings.Repeat("長い会話", 100)}, Object{"role": "user", "images": []any{Object{"data": "secret-image"}}}}, "context": Object{"secret": "private"}, "postBody": "private-post", "body": "private-body", "splitIssues": []any{Object{"project": "owner/repo", "purpose": "private-purpose"}}, "actions": []any{"chat"}}}, nil
			}
			return []Object{}, nil
		}
		f.options.Entries[kind] = func(context.Context, Object, string) (Object, error) {
			return Object{"id": fixtureID, "project": "owner/repo", "title": "相談", "body": "private-body", "status": "draft"}, nil
		}
	}
	for _, kind := range auxiliary {
		kind := kind
		f.options.Aux[kind] = func(context.Context) (any, error) { return []any{}, nil }
	}
	f.restart()
	return f
}
func (f *fixture) restart() {
	f.t.Helper()
	f.mux = http.NewServeMux()
	var e error
	f.s, e = Register(f.mux, f.options)
	if e != nil {
		f.t.Fatal(e)
	}
}
func (f *fixture) call(method, path, actor string, data any) (int, any) {
	f.t.Helper()
	raw, _ := json.Marshal(data)
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	if actor == "screen" {
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("X-CSRF-Token", "csrf")
	} else {
		r.Header.Set("Authorization", "Bearer "+f.token)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var result any
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		f.t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
	return w.Code, result
}
func TestScreenListProjectionAuthenticationAndCoreFailure(t *testing.T) {
	f := newFixture(t)
	code, value := f.call("GET", "/api/list", "screen", nil)
	if code != 200 {
		t.Fatal(code, value)
	}
	response := fleet.ObjectOrEmpty(value)
	rows := fleet.Items(response["rows"])
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	item := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(rows[0])["item"])
	raw, _ := json.Marshal(item)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "secret-image") || len([]rune(fleet.Text(fleet.ObjectOrEmpty(fleet.Items(item["messages"])[0])["text"]))) != 60 {
		t.Fatal("heavy or private detail leaked", item)
	}
	cursor := fleet.Text(response["cursor"])
	f.options.Lists["todo"] = func(context.Context, string) ([]Object, error) { return nil, errors.New("unavailable core") }
	f.s.o.Lists = f.options.Lists
	code, _ = f.call("GET", "/api/list?since="+cursor, "screen", nil)
	if code != 503 {
		t.Fatal("core read became empty success", code)
	}
	f.s.o.Lists["issue"] = func(context.Context, string) ([]Object, error) { return nil, errors.New("unavailable active core") }
	code, _ = f.call("GET", "/api/list?tabs=active", "screen", nil)
	if code != 503 {
		t.Fatal("active core read became empty success", code)
	}
	code, _ = f.call("GET", "/api/list?tabs=active&since=invalid!", "screen", nil)
	if code != 400 {
		t.Fatal("invalid cursor accepted", code)
	}
	code, _ = f.call("GET", "/api/list", "token", nil)
	if code != 403 {
		t.Fatal("token used session list", code)
	}
	before := f.sourceCalls.Load()
	code, _ = f.call("GET", "/api/list?since=bad&since=other", "screen", nil)
	if code != 400 || f.sourceCalls.Load() != before {
		t.Fatal("invalid query reached source")
	}
	code, value = f.call("GET", "/api/list/entry?type=draft&id="+fixtureID, "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(value)["item"])["body"] != nil {
		t.Fatal("entry not projected", code, value)
	}
	code, _ = f.call("GET", "/api/list/entry?type=issue&project=owner%2Frepo&number=0", "screen", nil)
	if code != 400 {
		t.Fatal("bad identifier accepted", code)
	}
}
func TestTopicRemovalKeepsDataAndWithdrawalMissingUsage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	put := func(id string, d Object) {
		t.Helper()
		raw, _ := json.Marshal(d)
		if _, e := f.db.DB().Exec(`INSERT INTO drafts(id,json) VALUES(?,?)`, id, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	put(fixtureID, Object{"id": fixtureID, "project": "owner/repo", "status": "draft", "topicLinks": []any{Object{"id": otherID}}, "duplicateHints": []any{Object{"label": "下書き " + otherID}}, "untouched": "保持"})
	put(otherID, Object{"id": otherID, "project": "owner/repo", "status": "submitted", "finishedWithoutIssue": true, "topicLinks": []any{Object{"id": fixtureID}}})
	code, value := f.call("GET", "/api/topic-links", "token", nil)
	if code != 200 || len(fleet.Items(fleet.ObjectOrEmpty(value)["groups"])) != 1 || fleet.ObjectOrEmpty(value)["reason"] == nil {
		t.Fatal("usage missing not explicit", code, value)
	}
	code, value = f.call("POST", "/api/drafts/"+fixtureID+"/topic-links/remove", "token", Object{"with": otherID})
	if code != 200 || fleet.ObjectOrEmpty(value)["removed"] != true {
		t.Fatal(code, value)
	}
	code, _ = f.call("POST", "/api/drafts/"+fixtureID+"/topic-links/remove", "token", Object{"with": otherID})
	if code != 200 {
		t.Fatal("removal not idempotent", code)
	}
	drafts, e := f.s.records(ctx, "drafts")
	if e != nil || drafts[fixtureID]["untouched"] != "保持" || drafts[fixtureID]["topicLinks"] != nil || drafts[fixtureID]["duplicateHints"] != nil || drafts[otherID]["topicLinks"] != nil {
		t.Fatal("record data not retained", drafts, e)
	}
	f.restart()
	code, value = f.call("GET", "/api/topic-links", "token", nil)
	if code != 200 || len(fleet.Items(fleet.ObjectOrEmpty(value)["groups"])) != 0 {
		t.Fatal("removed edge returned after restart", code, value)
	}
	if _, e = f.db.DB().Exec(`INSERT INTO draft_withdrawals(sequence,draft,project,title,operation,at,created_at,via,token_name,reason,parent) VALUES(1,?,'owner/repo','相談','todo','2026-10-31T23:00:00.000Z','2026-10-30T00:00:00.000Z','token','worker','整理',NULL)`, fixtureID); e != nil {
		t.Fatal(e)
	}
	code, value = f.call("GET", "/api/draft-withdrawals?from=2026-11-01&to=2026-11-01", "token", nil)
	if code != 200 || fleet.ObjectOrEmpty(fleet.Items(fleet.ObjectOrEmpty(value)["records"])[0])["used"] != nil || fleet.ObjectOrEmpty(value)["reason"] == nil {
		t.Fatal("withdrawal missing usage not marked", code, value)
	}
	f.s.o.Usage = func(context.Context, time.Time, time.Time) ([]Object, error) {
		return []Object{{"source": "intake", "draft": fixtureID, "at": "2026-10-31T22:00:00Z"}, {"source": "intake", "draft": fixtureID, "at": "2026-10-31T23:30:00Z"}}, nil
	}
	code, value = f.call("GET", "/api/draft-withdrawals?from=2026-11-01&to=2026-11-01", "token", nil)
	used := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleet.Items(fleet.ObjectOrEmpty(value)["records"])[0])["used"])
	if code != 200 || intValue(used["rows"]) != 1 || intValue(used["missing"]) != 1 || used["weighted"] != nil {
		t.Fatal("usage after operation counted or missing treated as zero", code, value)
	}
	code, _ = f.call("GET", "/api/draft-withdrawals?from=2026-02-30", "token", nil)
	if code != 400 {
		t.Fatal("invalid calendar date", code)
	}
}

func TestSummarizeDraftParentLinks(t *testing.T) {
	for _, field := range []string{"splitChildren", "consultationChildren"} {
		got := Summarize("draft", Object{"id": "parent", field: []any{"child"}, "parentConsultation": "ancestor", "body": "本文"})
		if got["hasChildren"] != true || got["parentConsultation"] != "ancestor" || got[field] != nil || got["body"] != nil {
			t.Fatalf("parent link projection: %v", got)
		}
		for _, children := range []any{nil, []any{}, []any{""}} {
			got = Summarize("draft", Object{field: children})
			if got["hasChildren"] != nil {
				t.Fatalf("empty link became a parent: %v", got)
			}
		}
	}
}

func TestSummarizeFamilyProgress(t *testing.T) {
	progress := Object{"total": 2, "completed": 1, "closed": 0, "remaining": 1, "stopped": []any{Object{"number": 3, "project": "owner/repo", "status": "failed", "reason": "failure"}}}
	got := Summarize("draft", Object{"familyProgress": progress, "status": "submitted"})
	if fleet.ObjectOrEmpty(got["familyProgress"])["remaining"] != 1 {
		t.Fatalf("progress missing: %v", got)
	}
}

func TestScreenListReturnsFamilyProgress(t *testing.T) {
	f := newFixture(t)
	f.s.o.Lists["draft"] = func(context.Context, string) ([]Object, error) {
		return []Object{{"id": fixtureID, "project": "owner/repo", "status": "submitted", "familyProgress": Object{"total": 2, "completed": 1, "closed": 0, "remaining": 1, "stopped": []any{Object{"project": "owner/repo", "number": 3, "status": "failed", "reason": "failure"}}}}}, nil
	}
	code, value := f.call("GET", "/api/list", "screen", nil)
	if code != 200 {
		t.Fatal(code, value)
	}
	rows := fleet.Items(fleet.ObjectOrEmpty(value)["rows"])
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	progress := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(rows[0])["item"])["familyProgress"])
	if progress["remaining"] != float64(1) || len(fleet.Items(progress["stopped"])) != 1 {
		t.Fatal(progress)
	}
}

func TestListMeasurementsAreReadableFromMeta(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/list", nil)
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	timing := w.Header().Get("Server-Timing")
	for _, name := range []string{"draft_read;dur=", "reviews_read;dur=", "delta;dur=", "json;dur="} {
		if !strings.Contains(timing, name) {
			t.Fatal("Server-Timing is missing", name, timing)
		}
	}
	var body Object
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	code, value := f.call("GET", "/api/list?since="+fleet.Text(body["cursor"]), "screen", nil)
	if code != 200 {
		t.Fatal(code, value)
	}
	code, value = f.call("POST", "/api/list/timing", "screen", Object{"fetchMs": 1200, "drawMs": 30, "firstDisplayMs": 1300, "encodedBytes": 4096, "decodedBytes": 90000, "rows": 1})
	if code != 200 {
		t.Fatal(code, value)
	}
	for _, bad := range []Object{{"fetchMs": -1}, {"fetchMs": "1"}, {"title": 1}} {
		if code, _ = f.call("POST", "/api/list/timing", "screen", bad); code != 400 {
			t.Fatal("invalid report accepted", bad, code)
		}
	}
	if code, _ = f.call("POST", "/api/list/timing", "token", Object{"fetchMs": 1}); code != 403 {
		t.Fatal("token reported a browser measurement", code)
	}
	code, value = f.call("GET", "/api/meta", "screen", nil)
	if code != 200 {
		t.Fatal(code, value)
	}
	measured := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(value)["listMeasurements"])
	full, delta, browser := fleet.Items(measured["full"]), fleet.Items(measured["delta"]), fleet.Items(measured["browser"])
	if len(full) != 1 || len(delta) != 1 || len(browser) != 1 {
		t.Fatal(measured)
	}
	first := fleet.ObjectOrEmpty(full[0])
	if first["bytes"] != float64(w.Body.Len()) || first["rows"] != float64(1) || first["status"] != float64(200) || fleet.ObjectOrEmpty(first["stages"])["draft_read"] == nil {
		t.Fatal("full measurement", first, w.Body.Len())
	}
	// The published measurements allow operators to compare every read with total time.
	total, ok := first["totalMs"].(float64)
	if !ok || total < 0 {
		t.Fatal("invalid total", first)
	}
	for _, kind := range rowTypes {
		name := kind + "_read"
		duration, ok := fleet.ObjectOrEmpty(first["stages"])[name].(float64)
		if !ok || duration < 0 || duration > total {
			t.Fatalf("invalid %s relative to total: %v / %v", name, duration, total)
		}
	}
	if fleet.ObjectOrEmpty(browser[0])["encodedBytes"] != float64(4096) || fleet.ObjectOrEmpty(browser[0])["fetchMs"] != float64(1200) {
		t.Fatal("browser measurement", browser[0])
	}
	raw, _ := json.Marshal(measured)
	if strings.Contains(string(raw), "相談") || strings.Contains(string(raw), fixtureID) {
		t.Fatal("row content leaked into measurements", string(raw))
	}
}

func TestSnapshotParallelOrderingAndTiming(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	s := &Service{o: Options{Config: func() (Object, error) { return Object{"repositories": []any{Object{"name": "owner/repo"}}}, nil }, Lists: map[string]ListReader{}, Aux: map[string]func(context.Context) (any, error){}}}
	for _, kind := range rowTypes {
		kind := kind
		s.o.Lists[kind] = func(ctx context.Context, origin string) ([]Object, error) {
			if kind == "issue" || kind == "draft" {
				started <- kind
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			switch kind {
			case "issue":
				return []Object{{"project": "owner/repo", "number": 1, "updatedAt": "1"}, {"project": "owner/repo", "number": 2, "updatedAt": "2"}}, nil
			case "draft":
				return []Object{{"project": "owner/repo", "id": fixtureID}}, nil
			case "todo":
				return []Object{{"project": "owner/repo", "id": otherID}}, nil
			default:
				return []Object{{"project": "owner/repo", "id": fixtureID, "status": "closing"}}, nil
			}
		}
	}
	for _, name := range append(append([]string{}, auxiliary...), "hosts") {
		name := name
		s.o.Aux[name] = func(context.Context) (any, error) {
			if name == "stalls" {
				return nil, errors.New("unavailable")
			}
			return name, nil
		}
	}
	timing := newListTiming()
	type result struct {
		snapshot Snapshot
		err      error
	}
	done := make(chan result, 1)
	go func() { v, e := s.snapshot(context.Background(), "", timing); done <- result{v, e} }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("readers did not start concurrently")
		}
	}
	close(release)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	keys := []string{}
	for _, row := range r.snapshot.Rows {
		keys = append(keys, row.Key)
	}
	if strings.Join(keys, ",") != "issue:owner/repo#2,issue:owner/repo#1,draft:"+fixtureID+",todo:"+otherID+",closeRequest:"+fixtureID {
		t.Fatal(keys)
	}
	if len(r.snapshot.Aux) != 3 || r.snapshot.Aux["reviews"] != "reviews" || r.snapshot.Aux["fullTests"] != "fullTests" || r.snapshot.Aux["attentionAlerts"] != "attentionAlerts" || r.snapshot.Hosts != "hosts" {
		t.Fatal(r.snapshot.Aux, r.snapshot.Hosts)
	}
	for _, name := range append(append(append([]string{}, rowTypes...), auxiliary...), "hosts") {
		if d, ok := timing.stages[name+"_read"]; !ok || d < 0 {
			t.Fatalf("%s: %v", name, d)
		}
	}
	if timing.stages["snapshot_prepare"] < 0 {
		t.Fatal(timing.stages)
	}
	t.Logf("fixed fixture stages: %v", timing.stages)
}

func TestSnapshotCancellationWaitsAndPreservesFailure(t *testing.T) {
	failure := errors.New("original failure")
	for _, tc := range []struct {
		name     string
		external bool
		failure  error
	}{{"failure", false, failure}, {"internal deadline", false, fmt.Errorf("todo read: %w", context.DeadlineExceeded)}, {"internal cancellation", false, context.Canceled}, {"external cancellation", true, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			external := tc.external
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &Service{o: Options{Config: func() (Object, error) { return Object{}, nil }, Lists: map[string]ListReader{}, Aux: map[string]func(context.Context) (any, error){}}}
			started := make(chan struct{}, len(rowTypes)+1)
			finished := make(chan struct{}, len(rowTypes)+1)
			trigger := make(chan struct{})
			for _, kind := range rowTypes {
				kind := kind
				s.o.Lists[kind] = func(ctx context.Context, _ string) ([]Object, error) {
					started <- struct{}{}
					defer func() { finished <- struct{}{} }()
					if kind == "todo" && !external {
						<-trigger
						return nil, tc.failure
					}
					<-ctx.Done()
					if kind == "issue" && !external {
						return nil, errors.New("driver canceled statement")
					}
					return nil, ctx.Err()
				}
			}
			s.o.Aux["hosts"] = func(ctx context.Context) (any, error) {
				started <- struct{}{}
				defer func() { finished <- struct{}{} }()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			done := make(chan error, 1)
			go func() { _, err := s.snapshot(ctx, "", newListTiming()); done <- err }()
			for i := 0; i < len(rowTypes)+1; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					cancel()
					close(trigger)
					t.Fatal("readers did not start")
				}
			}
			if external {
				cancel()
			} else {
				close(trigger)
			}
			select {
			case err := <-done:
				if external && !errors.Is(err, context.Canceled) || !external && !errors.Is(err, tc.failure) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not finish")
			}
			if len(finished) != len(rowTypes)+1 {
				t.Fatal("snapshot returned before readers finished")
			}
		})
	}
}

func TestScreenListScopes(t *testing.T) {
	f := newFixture(t)
	var todoCalls atomic.Int64
	var wrongScope atomic.Bool
	title := "before"
	expectedScope := "active"
	changedRows := false
	for _, kind := range rowTypes {
		kind := kind
		f.s.o.Lists[kind] = func(ctx context.Context, _ string) ([]Object, error) {
			if ListScope(ctx) != expectedScope {
				wrongScope.Store(true)
			}
			if kind == "todo" {
				todoCalls.Add(1)
			}
			if kind == "issue" {
				if changedRows {
					return []Object{{"project": "owner/repo", "number": 2, "title": title}}, nil
				}
				return []Object{{"project": "owner/repo", "number": 1, "title": title}}, nil
			}
			if kind == "closeRequest" {
				return []Object{{"id": otherID, "project": "owner/repo", "status": "reviewing"}}, nil
			}
			return []Object{{"id": fixtureID, "project": "owner/repo", "title": title}}, nil
		}
	}
	f.s.o.Aux["hosts"] = func(context.Context) (any, error) { return Object{"load": 1}, nil }
	call := func(path string) Object {
		t.Helper()
		code, v := f.call("GET", path, "screen", nil)
		if code != 200 {
			t.Fatal(code, v)
		}
		return fleet.ObjectOrEmpty(v)
	}
	a := call("/api/list?tabs=active")
	if a["scope"] != "active" || a["full"] != true || len(fleet.Items(a["rows"])) != 3 || todoCalls.Load() != 0 || a["hosts"] == nil || len(fleet.ObjectOrEmpty(a["aux"])) == 0 {
		t.Fatal(a, todoCalls.Load())
	}
	for _, row := range fleet.Items(a["rows"]) {
		if fleet.ObjectOrEmpty(row)["type"] == "todo" {
			t.Fatal(row)
		}
	}
	title = "after"
	changedRows = true
	d := call("/api/list?tabs=active&since=" + fleet.Text(a["cursor"]))
	if d["full"] != false || len(fleet.Items(d["updated"])) != 1 || len(fleet.Items(d["added"])) != 1 || len(fleet.Items(d["removed"])) != 1 {
		t.Fatal(d)
	}
	expectedScope = "full"
	full := call("/api/list?since=" + fleet.Text(d["cursor"]))
	if full["scope"] != "full" || full["reset"] != true || full["full"] != true || len(fleet.Items(full["rows"])) != 4 || todoCalls.Load() != 1 {
		t.Fatal(full)
	}
	expectedScope = "active"
	back := call("/api/list?tabs=active&since=" + fleet.Text(full["cursor"]))
	if back["reset"] != true || back["scope"] != "active" || todoCalls.Load() != 1 {
		t.Fatal(back)
	}
	expectedScope = "full"
	fresh := call("/api/list")
	if fresh["scope"] != "full" || len(fleet.Items(fresh["rows"])) != 4 {
		t.Fatal(fresh)
	}
	delta := call("/api/list?since=" + fleet.Text(fresh["cursor"]))
	if delta["full"] != false || delta["scope"] != "full" {
		t.Fatal(delta)
	}
	for _, query := range []string{"tabs=", "tabs=full", "tabs=active&tabs=active", "unknown=1", "since=x&since=y", "tabs=active&tabs=full;bad=1", "unknown=1;bad=2", "tabs=active&bad=%zz"} {
		before := todoCalls.Load()
		code, _ := f.call("GET", "/api/list?"+query, "screen", nil)
		if code != 400 || todoCalls.Load() != before {
			t.Fatal(query, code)
		}
	}
	if wrongScope.Load() {
		t.Fatal("invalid reader scope")
	}
	meta := call("/api/meta")
	measurements := fleet.ObjectOrEmpty(meta["listMeasurements"])
	found := map[string]bool{}
	for _, row := range fleet.Items(measurements["full"]) {
		found[fleet.Text(fleet.ObjectOrEmpty(row)["scope"])] = true
	}
	if !found["active"] || !found["full"] {
		t.Fatal(measurements)
	}
}
