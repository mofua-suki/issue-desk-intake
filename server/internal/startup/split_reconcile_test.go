package startup

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/live"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/store"
)

const splitParent = "11111111-1111-4111-8111-111111111111"
const splitChild = "22222222-2222-4222-8222-222222222222"

// splitCycleFixture uses the same PostgreSQL records, list route, filing cycle,
// family-progress readers and event publisher as the running application.
type splitCycleFixture struct {
	app           *App
	api           *recoveryAPI
	busy          string
	mux           *http.ServeMux
	events        *bufio.Reader
	eventResponse *http.Response
	server        *httptest.Server
	marker        int
}

func newSplitCycleFixture(t *testing.T) *splitCycleFixture {
	t.Helper()
	f := &splitCycleFixture{app: reviewNotesApp(t), api: &recoveryAPI{}}
	a, ctx := f.app, t.Context()
	var err error
	a.Auth, err = auth.New(auth.Config{PublicURL: "http://localhost:8787"}, &store.AuthStore{SQLite: a.SQLite, Postgres: a.DB})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err = a.SQLite.CreateSession(ctx, auth.Hash("fixture-session"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	a.Jobs, err = jobs.New(a.DB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.Numbers, err = numbers.OpenLedger(ctx, a.DB, filepath.Join(t.TempDir(), "numbers-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if _, err = a.Records.DB.ExecContext(ctx, `CREATE TABLE split_test_updates(id text NOT NULL); CREATE FUNCTION split_test_count_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO split_test_updates VALUES(NEW.id); RETURN NEW; END $$; CREATE TRIGGER split_test_count_update AFTER UPDATE ON drafts FOR EACH ROW EXECUTE FUNCTION split_test_count_update()`); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *splitCycleFixture) restart(t *testing.T) {
	t.Helper()
	a := f.app
	if f.eventResponse != nil {
		f.eventResponse.Body.Close()
		f.server.Close()
		a.Live.Close()
	}
	var err error
	a.Records, err = consultations.OpenRecords(t.Context(), a.SQLite.DB())
	if err != nil {
		t.Fatal(err)
	}
	a.Filing, err = filing.New(t.Context(), filing.Options{Records: a.Records, API: f.api, Jobs: a.Jobs, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) {
		t.Error("unexpected filing")
		return filing.Prepared{}, nil
	}, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	a.Consultations, err = consultations.New(consultations.Options{Records: a.Records, Auth: a.Auth, Project: func(string) error { return nil }, Active: func(id string) bool { return f.busy == id }, FamilyJobs: a.familyJobs})
	if err != nil {
		t.Fatal(err)
	}
	a.Live = live.New(t.Context(), a.Auth)
	f.mux = http.NewServeMux()
	a.Consultations.RegisterDrafts(f.mux)
	f.mux.Handle("/api/events", a.Live)
	f.server = httptest.NewServer(f.mux)
	req, err := http.NewRequestWithContext(t.Context(), "GET", f.server.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:8787"
	req.AddCookie(&http.Cookie{Name: "desk_session", Value: "fixture-session"})
	f.eventResponse, err = http.DefaultClient.Do(req)
	if err != nil || f.eventResponse.StatusCode != 200 {
		t.Fatalf("event subscription: %v %v", f.eventResponse, err)
	}
	f.events = bufio.NewReader(f.eventResponse.Body)
	if line, err := f.events.ReadString('\n'); err != nil || line != "retry: 3000\n" {
		t.Fatalf("event ready: %q %v", line, err)
	}
	t.Cleanup(func() { f.eventResponse.Body.Close(); f.server.Close(); a.Live.Close() })
}
func (f *splitCycleFixture) put(t *testing.T, d consultations.Record) {
	t.Helper()
	id := d["id"].(string)
	if _, err := f.app.Records.Change(t.Context(), "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
}
func (f *splitCycleFixture) get(t *testing.T, id string) consultations.Record {
	t.Helper()
	d, ok, err := f.app.Records.Get(t.Context(), "drafts", id)
	if err != nil || !ok {
		t.Fatalf("get %s: %v %v", id, ok, err)
	}
	return d
}
func (f *splitCycleFixture) updates(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.app.Records.DB.QueryContext(t.Context(), `SELECT id,count(*) FROM split_test_updates GROUP BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		m[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}
func (f *splitCycleFixture) list(t *testing.T, query string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+query, nil)
	req.AddCookie(&http.Cookie{Name: "desk_session", Value: "fixture-session"})
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("list: %d %s %v", w.Code, w.Body.String(), err)
	}
	return out.Items
}

// Two ordered sentinel events drain the SSE batch even when the map order of
// events in the first batch puts the sentinel before the event under test.
func (f *splitCycleFixture) drainEvents(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for range 2 {
		f.marker++
		marker := f.marker
		f.app.Publish(map[string]any{"type": "issue", "project": "barrier/test", "number": marker})
		for {
			line, err := f.events.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var e map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				t.Fatal(err)
			}
			if e["project"] == "barrier/test" && e["number"] == float64(marker) {
				break
			}
			out = append(out, e)
		}
	}
	return out
}
func (f *splitCycleFixture) nextEvent(t *testing.T) map[string]any {
	t.Helper()
	for {
		line, err := f.events.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			return event
		}
	}
}
func (f *splitCycleFixture) cycle(t *testing.T) {
	t.Helper()
	if err := f.app.filingPendingCycle(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func splitFamily(parent, child, project string) (consultations.Record, consultations.Record) {
	return consultations.Record{"id": parent, "project": project, "status": "submit_error", "revision": 0, "splitChildren": []any{child}, "splitIssues": []any{consultations.Record{}}, "error": "filing failed", "errorCode": "FAILED"}, consultations.Record{"id": child, "project": project, "parentSplit": parent, "status": "submitted", "revision": 0, "issue": consultations.Record{"number": 5}}
}
func classifySplitRows(t *testing.T, drafts []map[string]any) []map[string]any {
	t.Helper()
	data, err := json.Marshal(drafts)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "--input-type=module", "-e", `import {requestRows,monitoringRequired,attentionClassification} from '../../../shared/src/desk-attention.mts'; let s=''; for await (const c of process.stdin) s+=c; console.log(JSON.stringify(requestRows([],JSON.parse(s)).map(r=>({...r,monitoring:monitoringRequired(r),classification:attentionClassification(r)}))))`)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shared classification: %v %s", err, out)
	}
	var rows []map[string]any
	if err = json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
func rowCount(rows []map[string]any, id, status string) int {
	n := 0
	for _, r := range rows {
		if r["id"] == id && r["status"] == status {
			n++
		}
	}
	return n
}
func monitoringCount(rows []map[string]any, id, status string) int {
	n := 0
	for _, row := range rows {
		if row["id"] == id && row["status"] == status && row["monitoring"] == true {
			n++
		}
	}
	return n
}

func TestSplitParentReconcilePeriodicAndList(t *testing.T) {
	f := newSplitCycleFixture(t)
	p, c := splitFamily(splitParent, splitChild, "owner/target")
	f.put(t, p)
	f.put(t, c)
	// A queued implementation job demonstrates that submitted means filing, not merge.
	if _, err := f.app.DB.ExecContext(t.Context(), `INSERT INTO jobs(repo,issue,status,result,view) VALUES('owner/target',5,'queued','{}','{}')`); err != nil {
		t.Fatal(err)
	}
	otherIDs := []string{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"}
	childIDs := []string{"66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888"}
	for i, id := range otherIDs {
		p, c := splitFamily(id, childIDs[i], "owner/other")
		switch i {
		case 0:
			c["status"] = "draft"
			delete(c, "issue")
		case 1:
			c["status"] = "thinking"
		case 2:
			p["_groupLinkGroups"] = []any{[]any{"owner/a", "owner/b"}}
			p["_groupLinksComplete"] = false
		}
		f.put(t, p)
		f.put(t, c)
	}
	other := "99999999-9999-4999-8999-999999999999"
	f.put(t, consultations.Record{"id": other, "project": "owner/other", "status": "error", "revision": 0})
	beforeChildren := map[string]consultations.Record{}
	for _, id := range append([]string{splitChild}, childIDs...) {
		beforeChildren[id] = f.get(t, id)
	}
	before := f.list(t, "?status=submit_error&project=owner%2Ftarget")
	if len(before) != 1 {
		t.Fatalf("before=%v", before)
	}
	rows := classifySplitRows(t, f.list(t, ""))
	if rowCount(rows, splitParent, "submit_error") != 0 || rowCount(rows, childIDs[0], "draft") != 1 || rowCount(rows, childIDs[1], "pending") != 1 {
		t.Fatalf("before rows=%v", rows)
	}
	if got := f.updates(t); len(got) != 0 {
		t.Fatalf("read caused writes: %v", got)
	}
	f.app.startFilingPending()
	if ev := f.nextEvent(t); ev["id"] != splitParent || ev["type"] != "draft" {
		t.Fatalf("periodic event=%v", ev)
	}
	f.app.StopBackground()
	if got := f.updates(t); !reflect.DeepEqual(got, map[string]int{splitParent: 1}) {
		t.Fatalf("updates=%v", got)
	}
	if ev := f.drainEvents(t); len(ev) != 0 {
		t.Fatalf("events=%v", ev)
	}
	if got := f.get(t, splitParent); got["status"] != "submitted" || got["error"] != nil || got["errorCode"] != nil {
		t.Fatalf("parent=%v", got)
	}
	after := f.list(t, "?status=submit_error&project=owner%2Ftarget")
	if len(after) != 0 {
		t.Fatalf("after=%v", after)
	}
	for _, id := range otherIDs {
		if d := f.get(t, id); d["status"] != "submit_error" {
			t.Fatalf("protected %s: %v", id, d)
		}
	}
	if d := f.get(t, other); d["status"] != "error" {
		t.Fatalf("other error lost: %v", d)
	}
	for id, want := range beforeChildren {
		if got := f.get(t, id); !reflect.DeepEqual(got, want) {
			t.Fatalf("child changed %s: %v", id, got)
		}
	}
	rows = classifySplitRows(t, f.list(t, ""))
	if monitoringCount(rows, splitParent, "submit_error") != 0 || rowCount(rows, splitParent, "family_in_progress") != 0 || rowCount(rows, other, "error") != 1 || monitoringCount(rows, other, "error") != 0 {
		t.Fatalf("after rows=%v", rows)
	}
	for _, row := range rows {
		if row["id"] == other && row["classification"] != "handoff" {
			t.Fatalf("error consultation must be handed off: %v", row)
		}
	}
	for _, id := range otherIDs {
		if monitoringCount(rows, id, "submit_error") != 0 {
			t.Fatalf("protected classification %s: %v", id, rows)
		}
	}
	if rowCount(rows, childIDs[0], "draft") != 1 || rowCount(rows, childIDs[1], "pending") != 1 {
		t.Fatalf("independent children=%v", rows)
	}
	if f.api.calls != 0 {
		t.Fatalf("external calls=%d", f.api.calls)
	}
	if got := f.updates(t); !reflect.DeepEqual(got, map[string]int{splitParent: 1}) {
		t.Fatalf("read caused writes: %v", got)
	}
	// A previously incomplete child is filed later; the next cycle revisits its parent.
	if _, err := f.app.Records.Change(t.Context(), "drafts", childIDs[0], func(_ *sql.Tx, d consultations.Record) (consultations.Record, error) {
		d["status"] = "submitted"
		d["issue"] = consultations.Record{"number": 6}
		return d, nil
	}); err != nil {
		t.Fatal(err)
	}
	f.cycle(t)
	if got := f.updates(t); !reflect.DeepEqual(got, map[string]int{splitParent: 1, childIDs[0]: 1, otherIDs[0]: 1}) {
		t.Fatalf("late filing updates=%v", got)
	}
	if d := f.get(t, otherIDs[0]); d["status"] != "submitted" {
		t.Fatalf("late filing parent=%v", d)
	}
	if ev := f.drainEvents(t); len(ev) != 1 || ev[0]["id"] != otherIDs[0] {
		t.Fatalf("late filing events=%v", ev)
	}
}

func TestSplitParentReconcileBusyThenRetry(t *testing.T) {
	for _, busyMember := range []string{splitParent, splitChild} {
		t.Run(busyMember, func(t *testing.T) {
			f := newSplitCycleFixture(t)
			p, c := splitFamily(splitParent, splitChild, "owner/target")
			f.put(t, p)
			f.put(t, c)
			f.busy = busyMember
			f.cycle(t)
			if got := f.updates(t); len(got) != 0 {
				t.Fatalf("busy updates=%v", got)
			}
			if ev := f.drainEvents(t); len(ev) != 0 {
				t.Fatalf("busy events=%v", ev)
			}
			f.busy = ""
			f.cycle(t)
			if got := f.updates(t); !reflect.DeepEqual(got, map[string]int{splitParent: 1}) {
				t.Fatalf("retry updates=%v", got)
			}
			if ev := f.drainEvents(t); len(ev) != 1 || ev[0]["id"] != splitParent {
				t.Fatalf("retry events=%v", ev)
			}
			f.cycle(t)
			if ev := f.drainEvents(t); len(ev) != 0 {
				t.Fatalf("duplicate events=%v", ev)
			}
			f.restart(t)
			f.cycle(t)
			if ev := f.drainEvents(t); len(ev) != 0 {
				t.Fatalf("restart events=%v", ev)
			}
			if got := f.updates(t); !reflect.DeepEqual(got, map[string]int{splitParent: 1}) {
				t.Fatalf("duplicate updates=%v", got)
			}
			if d := f.get(t, splitParent); d["status"] != "submitted" || fmt.Sprint(d["revision"]) != "1" {
				t.Fatalf("parent=%v", d)
			}
			if f.api.calls != 0 {
				t.Fatalf("external calls=%d", f.api.calls)
			}
		})
	}
}

type cycleIssueAPI struct{ *recoveryAPI }

func (a *cycleIssueAPI) Issue(_ context.Context, _ string, n int64) (map[string]any, error) {
	if n == 1 {
		return nil, fmt.Errorf("issue 1 unavailable")
	}
	return map[string]any{"number": n, "state": "open", "title": "healthy"}, nil
}

func TestFilingCycleResolvesDependenciesDespitePendingFailure(t *testing.T) {
	f := newSplitCycleFixture(t)
	ctx := t.Context()
	var err error
	f.app.Filing, err = filing.New(ctx, filing.Options{Records: f.app.Records, API: &cycleIssueAPI{f.api}, Jobs: f.app.Jobs, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	refs := []any{map[string]any{"repo": "owner/app", "issue": 2}}
	dependent, err := f.app.Jobs.Create(ctx, jobs.NewJob{Repo: "owner/app", Issue: 3, Scope: []string{"c.go"}, View: map[string]any{"dependsOnIssues": refs, "unresolvedDependencies": refs}})
	if err != nil {
		t.Fatal(err)
	}
	for n := int64(1); n <= 2; n++ {
		raw, err := json.Marshal(map[string]any{"repo": "owner/app", "number": n, "draftId": splitChild, "preparedJob": jobs.NewJob{Repo: "owner/app", Issue: n, Title: "test", Scope: []string{"a.go"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.app.Records.DB.ExecContext(ctx, "INSERT INTO job_filings(key,json) VALUES(?,?)", fmt.Sprintf("owner/app#%d", n), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.app.filingPendingCycle(ctx); err == nil || !strings.Contains(err.Error(), "issue 1 unavailable") {
		t.Fatalf("cycle error=%v", err)
	}
	predecessor, err := f.app.Jobs.ByIssue(ctx, "owner/app", 2)
	if err != nil || predecessor == nil {
		t.Fatalf("predecessor=%v err=%v", predecessor, err)
	}
	var count int
	if err = f.app.Records.DB.QueryRowContext(ctx, "SELECT count(*) FROM job_filings WHERE key=?", "owner/app#1").Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending=%d err=%v", count, err)
	}
	detail, err := f.app.Jobs.Detail(ctx, dependent.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	deps := detail["dependsOn"].([]int64)
	if len(deps) != 1 || deps[0] != predecessor.ID {
		t.Fatalf("dependencies=%v", deps)
	}
	job, err := f.app.Jobs.Get(ctx, dependent.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := jobs.DependenciesReady(ctx, f.app.DB, job.ID, job.View)
	if err != nil || ready {
		t.Fatalf("unfinished ready=%v err=%v", ready, err)
	}
	if _, err = f.app.DB.ExecContext(ctx, "UPDATE jobs SET status='merged' WHERE id=$1", predecessor.ID); err != nil {
		t.Fatal(err)
	}
	ready, err = jobs.DependenciesReady(ctx, f.app.DB, job.ID, job.View)
	if err != nil || !ready {
		t.Fatalf("merged ready=%v err=%v", ready, err)
	}
}
