package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/deskcleanup"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/store"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func postgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQLのDSNがありません")
	}
	u, e := url.Parse(dsn)
	if e != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用試験DBが必要です")
	}
	admin, e := store.OpenPostgres(context.Background(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("ops_%d", time.Now().UnixNano())
	if _, e = admin.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, e := store.OpenPostgres(context.Background(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close(); admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	if _, e = store.Migrate(context.Background(), db); e != nil {
		t.Fatal(e)
	}
	return db
}
func TestPostgresIncidentActionUsesSharedSettingsAndDeadline(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	ctx := context.Background()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	raw, _ := json.Marshal([]any{map[string]any{"id": id, "key": "case", "kind": "stall", "state": "waiting", "deadline": "2026-11-01T00:01:00.000Z", "history": []any{}}})
	if _, e := s.PostgreSQL.Exec("INSERT INTO settings(key,value,updated_by)VALUES('incident-deadline',$1::jsonb,'test')", string(raw)); e != nil {
		t.Fatal(e)
	}
	if _, e := s.IncidentAction(ctx, id, "acknowledge", "nas", map[string]any{}); e == nil {
		t.Fatal("waiting acknowledged")
	}
	r, e := s.IncidentAction(ctx, id, "respond", "", map[string]any{})
	if e != nil || fleet.Object(r)["state"] != "user" {
		t.Fatal(r, e)
	}
	if _, e = s.IncidentAction(ctx, id, "acknowledge", "nas", map[string]any{}); e != nil {
		t.Fatal(e)
	}
	value, e := s.OperationalSetting(ctx, "incident-deadline")
	if e != nil || fleet.Object(fleet.List(value)[0])["acknowledgedAt"] == nil {
		t.Fatal(value, e)
	}
	var local int
	if e = s.DB.QueryRow("SELECT count(*)FROM settings WHERE id='incident-deadline'").Scan(&local); e != nil || local != 0 {
		t.Fatal("incident written into unrelated application settings")
	}
	raw, _ = json.Marshal([]any{map[string]any{"id": id, "state": "waiting", "deadline": "2026-11-01T00:00:00.000Z", "history": []any{}}})
	s.PostgreSQL.Exec("UPDATE settings SET value=$1::jsonb WHERE key='incident-deadline'", string(raw))
	if _, e = s.IncidentAction(ctx, id, "respond", "", map[string]any{}); e == nil {
		t.Fatal("deadline equality accepted")
	}
}
func TestPostgresHostAndDispatchAlertsUseRealState(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.StartedAt = s.now().Add(-2 * time.Hour)
	s.Assignments = &fleet.Assignments{Hosts: map[string]*fleet.Host{"worker": {Max: 4}}, Implementers: map[string][]string{"owner/repo": {"worker"}}, Output: map[string]any{}}
	ctx := context.Background()
	if _, e := s.PostgreSQL.Exec("INSERT INTO hosts(host,heartbeat_at,ready,capacity,implementing,publishing)VALUES('worker',$1,true,4,0,4)", s.now()); e != nil {
		t.Fatal(e)
	}
	if _, e := s.PostgreSQL.Exec("INSERT INTO jobs(repo,issue,title,scope,status,updated_at)VALUES('owner/repo',1,'waiting',ARRAY['a.go'],'queued',$1)", s.now().Add(-90*time.Minute)); e != nil {
		t.Fatal(e)
	}
	rows, e := s.Stalls(ctx)
	if e != nil {
		t.Fatal(e)
	}
	kinds := map[string]bool{}
	for _, r := range rows {
		kinds[fleet.Text(fleet.Object(r)["kind"])] = true
	}
	if !kinds["dispatch_idle"] || !kinds["hostd_stalled"] || kinds["job_dispatch_stall"] {
		t.Fatal(rows)
	}
	assertFull := func(kind string, rows []any) {
		t.Helper()
		for _, raw := range rows {
			row := fleet.Object(raw)
			if row["kind"] == kind {
				detail := fleet.Text(row["detail"])
				if !strings.Contains(detail, "報告値では枠が埋まっている") || strings.Contains(detail, "受け取り可と報告") {
					t.Fatal(detail)
				}
				return
			}
		}
		t.Fatal(kind, rows)
	}
	assertFull("dispatch_idle", rows)
	if _, e = s.PostgreSQL.Exec(`UPDATE hosts SET ready=false, checks='{"ok":true,"items":[{"name":"docker","ok":true}]}' WHERE host='worker'`); e != nil {
		t.Fatal(e)
	}
	rows, e = s.Stalls(ctx)
	if e != nil {
		t.Fatal(e)
	}
	foundReady := false
	for _, raw := range rows {
		row := fleet.Object(raw)
		if row["id"] == "host-check-worker" {
			t.Fatal(row)
		}
		if row["kind"] == "dispatch_idle" && strings.Contains(fleet.Text(row["detail"]), "ready=false") {
			foundReady = true
		}
	}
	if !foundReady {
		t.Fatal(rows)
	}
	if _, e = s.PostgreSQL.Exec("UPDATE hosts SET ready=true WHERE host='worker'"); e != nil {
		t.Fatal(e)
	}

	if _, e = s.PostgreSQL.Exec("INSERT INTO jobs(repo,issue,title,status)VALUES('owner/repo',2,'finished','merged')"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PostgreSQL.Exec("INSERT INTO job_events(job_id,kind,host,at,data)SELECT id,'leased','worker',$1,'{}' FROM jobs WHERE issue=2", s.now().Add(-10*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) SELECT id,'dispatch_skip','worker',$1,'{"reason":"scope_reserved","by":2}' FROM jobs WHERE issue=1`, s.now().Add(-time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) SELECT id,'dispatch_skip','worker',$1,'{"reason":"overlap","by":2}' FROM jobs WHERE issue=1`, s.now()); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) SELECT id,'dispatch_skip','worker',$1,'{"reason":"future_reason","preferredHost":"private-value"}' FROM jobs WHERE issue=1`, s.now().Add(-2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	rows, e = s.Stalls(ctx)
	if e != nil {
		t.Fatal(e)
	}
	kinds = map[string]bool{}
	for _, r := range rows {
		kinds[fleet.Text(fleet.Object(r)["kind"])] = true
	}
	if kinds["dispatch_idle"] || !kinds["job_dispatch_stall"] {
		t.Fatal(rows)
	}
	assertFull("job_dispatch_stall", rows)
	for _, raw := range rows {
		if fleet.Object(raw)["kind"] == "job_dispatch_stall" {
			detail := fleet.Text(fleet.Object(raw)["detail"])
			if !strings.Contains(detail, "future_reason") || strings.Contains(detail, "scope_reserved") || strings.Contains(detail, "private-value") {
				t.Fatal(detail)
			}
			assertGeneratedStallRemainsClosed(t, s, fleet.Object(raw), func() ([]any, error) { return s.Stalls(ctx) })
		}
	}
}

type urgentIssueReader struct{}

type slowUrgentReader struct{ calls atomic.Int64 }

func (r *slowUrgentReader) Do(ctx context.Context, _, _ string, _ any, _ any) error {
	r.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestPostgresUrgentHoldsBoundsSlowIssueReads(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	reader := &slowUrgentReader{}
	s.Gitea = reader
	for i := 1; i <= 100; i++ {
		if _, err := s.PostgreSQL.Exec("INSERT INTO jobs(repo,issue,title,status,priority,updated_at)VALUES('owner/repo',$1,'closed','failed',1,$2)", i, s.now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	rows, err := s.urgentHolds(t.Context())
	if err != nil || len(rows) != 50 || reader.calls.Load() < 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("rows=%d calls=%d elapsed=%s err=%v", len(rows), reader.calls.Load(), time.Since(start), err)
	}
}

func (urgentIssueReader) Do(_ context.Context, _, endpoint string, _ any, out any) error {
	if strings.HasSuffix(endpoint, "/3") {
		return fmt.Errorf("unavailable")
	}
	state := "open"
	if strings.HasSuffix(endpoint, "/1") {
		state = "closed"
	}
	*(out.(*map[string]any)) = map[string]any{"state": state}
	return nil
}

func TestPostgresUrgentHoldsIssueState(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.Gitea = urgentIssueReader{}
	for i := 1; i <= 3; i++ {
		if _, err := s.PostgreSQL.Exec("INSERT INTO jobs(repo,issue,title,status,priority,updated_at)VALUES('owner/repo',$1,'test','failed',1,$2)", i, s.now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.urgentHolds(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, raw := range rows {
		detail := fleet.Text(fleet.Object(raw)["detail"])
		if strings.Contains(detail, "owner/repo#1 ") {
			t.Fatal("closed issue shown", detail)
		}
	}
	if !strings.Contains(fleet.Text(fleet.Object(rows[0])["detail"]), "owner/repo#2 ") || !strings.Contains(fleet.Text(fleet.Object(rows[1])["detail"]), "owner/repo#3 ") {
		t.Fatal(rows)
	}
	assertGeneratedStallRemainsClosed(t, s, fleet.Object(rows[0]), func() ([]any, error) { return s.urgentHolds(t.Context()) })
}

func assertGeneratedStallRemainsClosed(t *testing.T, s *Service, row map[string]any, generate func() ([]any, error)) {
	t.Helper()
	cleanup := &deskcleanup.Service{DB: s.DB, Project: func(string) error { return nil }, Alerts: func(context.Context, string) ([]any, error) { return generate() }}
	target := "stall:" + fleet.Text(row["id"])
	candidate, err := cleanup.Inspect(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	before := s.now()
	s.Now = func() time.Time { return before.Add(time.Minute) }
	_, err = cleanup.Apply(t.Context(), deskcleanup.Object{"target": target, "fingerprint": candidate["fingerprint"], "reason": "終了", "userStatement": "終了してください"}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	visible, err := deskcleanup.VisibleAlerts(t.Context(), s.DB, "stall", rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range visible {
		if fleet.Object(raw)["id"] == row["id"] {
			t.Fatal("経過後に再表示", raw)
		}
	}
}

func TestPostgresRCAlertCanBeClosed(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	if _, err := s.PostgreSQL.Exec("INSERT INTO hosts(host,heartbeat_at,ready,capacity,implementing,publishing)VALUES('a9max',$1,true,1,0,0)", s.now()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.rcAlerts(t.Context())
	if err != nil || len(rows) != 1 || fleet.Object(rows[0])["project"] != fleet.SystemProject {
		t.Fatal(rows, err)
	}
	cleanup := &deskcleanup.Service{DB: s.DB, Project: func(string) error { return fmt.Errorf("no configured repositories") }, Alerts: func(context.Context, string) ([]any, error) { return rows, nil }}
	candidate, err := cleanup.Inspect(t.Context(), "attention:rc-update:a9max")
	if err != nil {
		t.Fatal(err)
	}
	result, err := cleanup.Apply(t.Context(), deskcleanup.Object{"target": candidate["target"], "fingerprint": candidate["fingerprint"], "reason": "終了", "userStatement": "終了してください"}, auth.Principal{Kind: auth.Screen})
	if err != nil || result["closed"] != true {
		t.Fatal(result, err)
	}
	visible, err := deskcleanup.VisibleAlerts(t.Context(), s.DB, "attention", rows)
	if err != nil || len(visible) != 0 {
		t.Fatal(visible, err)
	}
}

func TestPostgresReservationSkipHistoryDetail(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.Assignments = &fleet.Assignments{Implementers: map[string][]string{"owner/repo": {"worker"}}}
	now := s.now()
	ctx := context.Background()
	_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,title,status,filed_at,updated_at) VALUES (101,'owner/repo',11,'待機','queued',$1,$2),(102,'owner/repo',22,'予約元','merged',$1,$2)`, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES (102,'leased','worker',$1,'{}'),(102,'control','worker',$2,'{"action":"retry"}'),(102,'lease_expired','worker',$3,'{"to":"queued"}'),(102,'requeued','worker',$4,'{"from":"scope_wait"}'),(101,'dispatch_skip','worker',$5,'{"reason":"scope_reserved","by":102}'),(102,'control','worker',$5,'{"action":"retry"}')`, now.Add(-150*time.Minute), now.Add(-140*time.Minute), now.Add(-130*time.Minute), now.Add(-100*time.Minute), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES (102,'leased','worker',$1,'{}')`, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	list := []map[string]any{{"id": float64(101), "repo": "owner/repo", "updated_at": now.Add(-2 * time.Hour).Format(time.RFC3339Nano)}}
	detail := func() string {
		t.Helper()
		out, e := s.jobDispatchAlerts(ctx, list, map[string]map[string]any{}, 20)
		if e != nil || len(out) != 1 {
			t.Fatal(out, e)
		}
		return fleet.Text(fleet.Object(out[0])["detail"])
	}
	got := detail()
	for _, want := range []string{"過去の予約による見送り", "ジョブID: 102", "owner/repo", "#22", "予約元", "待ち時間: 40分", iso(now.Add(-time.Hour))} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	for _, tc := range []struct{ data, want, absent string }{{`{"reason":"scope_reserved"}`, "予約元: 不明", "ジョブID:"}, {`{"reason":"scope_reserved","by":999}`, "ジョブID: 999（取得不可）", "#22"}, {`{"reason":"overlap"}`, "overlap", "過去の予約による見送り"}} {
		_, err = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES (101,'dispatch_skip','worker',$1,$2::jsonb)`, now, tc.data)
		if err != nil {
			t.Fatal(err)
		}
		got = detail()
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.absent) {
			t.Fatal(got)
		}
	}
	_, err = s.PostgreSQL.Exec(`DELETE FROM job_events WHERE job_id=101 AND kind='dispatch_skip'`)
	if err != nil {
		t.Fatal(err)
	}
	got = detail()
	if !strings.Contains(got, "見送りの記録なし") || strings.Contains(got, "過去の予約による見送り") {
		t.Fatal(got)
	}
}
