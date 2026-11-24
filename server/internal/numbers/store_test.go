package numbers

import (
	"context"
	"database/sql"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

func fixture(t *testing.T) *Store {
	t.Helper()
	previousDialect := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previousDialect) })
	s, err := OpenLedger(context.Background(), pgtest.New(t), filepath.Join(t.TempDir(), "intake-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestInitialAssignmentRequiresFullInventoryAndStableOrder(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	lists := []List{{Project: "owner/b", Issues: []Candidate{{Number: 8, CreatedAt: "2026-02-01"}}}, {Project: "owner/a", Issues: []Candidate{{Number: 2, CreatedAt: "2026-01-01"}, {Number: 100, PullRequest: true}}}}
	if ok, err := s.Assign(ctx, lists[:1], []string{"owner/a", "owner/b"}, true); err != nil || ok {
		t.Fatal("incomplete initial inventory accepted")
	}
	if ok, err := s.Assign(ctx, lists, []string{"owner/a", "owner/b"}, true); err != nil || !ok {
		t.Fatal(err)
	}
	first, found, err := s.Number(ctx, "owner/a", 2)
	if err != nil || !found || first != 9 {
		t.Fatalf("%d %v %v", first, found, err)
	}
	second, _, _ := s.Number(ctx, "owner/b", 8)
	if second != 10 {
		t.Fatal(second)
	}
	if _, err = s.Assign(ctx, lists, nil, true); err != nil {
		t.Fatal(err)
	}
	again, _, _ := s.Number(ctx, "owner/a", 2)
	if again != first {
		t.Fatal("number reassigned")
	}
	ready, err := s.Ready(ctx, []string{"owner/a", "owner/b"})
	if err != nil || !ready {
		t.Fatal(err)
	}
}
func TestOldDatabaseRestoresJournalAndNeverReusesConflict(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if _, err := s.DB.Exec("INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(10,'owner/current',1,'now')"); err != nil {
		t.Fatal(err)
	}
	data := `{"number":10,"project":"owner/old","gitea":1,"at":"then"}
{"number":20,"project":"owner/missing","gitea":2,"at":"then"}
{"next":50,"at":"then"}
{"broken":`
	if err := os.WriteFile(s.Journal, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	check, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Restored) != 1 || check.Restored[0] != 20 || len(check.Conflicts) != 1 || check.Conflicts[0] != 10 || check.Invalid != 1 || check.Next == nil || *check.Next != 50 {
		t.Fatalf("%+v", check)
	}
	if check.Recorded != 1 {
		t.Fatal("database-only number not journaled")
	}
	next, err := s.Reserve(ctx, 30)
	if err != nil || next != 50 {
		t.Fatal(next, err)
	}
	check, err = s.Reconcile(ctx)
	if err != nil || check.Next == nil || *check.Next != 50 {
		t.Fatalf("%+v %v", check, err)
	}
}
func TestRenameJournalRecoversSameNumber(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	s.Assign(ctx, []List{{Project: "owner/old", Issues: []Candidate{{Number: 3}}}}, []string{"owner/old"}, true)
	if err := s.Rename(ctx, "owner/old", "owner/new"); err != nil {
		t.Fatal(err)
	}
	number, found, err := s.Number(ctx, "owner/new", 3)
	if err != nil || !found {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DELETE FROM desk_numbers"); err != nil {
		t.Fatal(err)
	}
	check, err := s.Reconcile(ctx)
	if err != nil || len(check.Restored) != 1 || check.Restored[0] != number {
		t.Fatalf("%+v %v", check, err)
	}
	issue, found, err := s.Issue(ctx, number)
	if err != nil || !found || issue.Project != "owner/new" {
		t.Fatalf("%+v %v", issue, err)
	}
}
func TestJournalFailureRollsBackAssignment(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s.Journal = t.TempDir()
	if _, err := s.Assign(ctx, []List{{Project: "owner/a", Issues: []Candidate{{Number: 1}}}}, []string{"owner/a"}, true); err == nil {
		t.Fatal("journal failure ignored")
	}
	if _, found, err := s.Number(ctx, "owner/a", 1); err != nil || found {
		t.Fatal("unrecoverable number committed")
	}
	if ready, _ := s.Ready(ctx, []string{"owner/a"}); ready {
		t.Fatal("failed scan marked ready")
	}
}

func TestFailedCommitCannotReuseAJournaledNumber(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`CREATE TABLE gate(id BIGINT PRIMARY KEY);CREATE TABLE deferred_gate(id BIGINT REFERENCES gate(id) DEFERRABLE INITIALLY DEFERRED);CREATE FUNCTION fail_counter_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO deferred_gate VALUES(1); RETURN NEW; END $$;CREATE TRIGGER fail_counter AFTER INSERT ON desk_counter FOR EACH ROW EXECUTE FUNCTION fail_counter_fn();`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assign(ctx, []List{{Project: "owner/original", Issues: []Candidate{{Number: 1}}}}, []string{"owner/original"}, true); err == nil {
		t.Fatal("deferred commit did not fail")
	}
	if _, err := s.DB.Exec("DROP TRIGGER fail_counter ON desk_counter"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assign(ctx, []List{{Project: "owner/other", Issues: []Candidate{{Number: 1}}}}, []string{"owner/other"}, true); err != nil {
		t.Fatal(err)
	}
	original, found, err := s.Number(ctx, "owner/original", 1)
	if err != nil || !found {
		t.Fatal("journaled assignment not recovered", err)
	}
	other, found, err := s.Number(ctx, "owner/other", 1)
	if err != nil || !found || other <= original {
		t.Fatalf("number reused after rollback: original=%d other=%d %v", original, other, err)
	}
}

func TestPostgresIssueNumbersMatchFull(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES (1,'owner/repo',1,'now'),(2,'OWNER/REPO',1,'now'),(3,'outside/repo',9,'now')`); err != nil {
		t.Fatal(err)
	}
	issues := []Issue{{"owner/repo", 1}, {"Owner/Repo", 1}, {"owner/repo", 2}, {"missing/repo", 99}, {"owner/repo", 1}}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Issue]int64{}
	for _, issue := range issues {
		if n, ok := all[issue]; ok {
			want[issue] = n
		}
	}
	got, err := s.ForIssues(ctx, issues)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got) != 1 {
		t.Fatalf("got=%v want=%v", got, want)
	}
	empty, err := (&Store{}).ForIssues(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty=%v err=%v", empty, err)
	}
}

type sqlCounts struct {
	selects, inserts, updates int
	enabled                   bool
}
type queryTracer struct{ counts *sqlCounts }

func (t queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t.counts.enabled {
		q := strings.ToLower(strings.TrimSpace(data.SQL))
		if strings.HasPrefix(q, "select") && strings.Contains(q, "desk_numbers") {
			t.counts.selects++
		}
		if strings.HasPrefix(q, "insert") {
			t.counts.inserts++
		}
		if strings.HasPrefix(q, "update") {
			t.counts.updates++
		}
	}
	return ctx
}
func (queryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func countedFixture(t *testing.T) (*Store, *sqlCounts) {
	t.Helper()
	previous := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previous) })
	env := pgtest.NewIsolated(t)
	config, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = env.Schema
	counts := &sqlCounts{}
	config.Tracer = queryTracer{counts}
	db := sql.OpenDB(appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)))
	t.Cleanup(func() { db.Close() })
	s, err := OpenLedger(context.Background(), db, filepath.Join(t.TempDir(), "intake-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s, counts
}
func assignOK(t *testing.T, s *Store, lists []List) {
	t.Helper()
	ok, err := s.Assign(context.Background(), lists, nil, true)
	if err != nil || !ok {
		t.Fatalf("Assign: %v %v", ok, err)
	}
}
func candidates(n int) []Candidate {
	out := make([]Candidate, n)
	for i := range out {
		out[i] = Candidate{Number: int64(i + 1)}
	}
	return out
}
func snapshot(t *testing.T, s *Store) (int64, int, string) {
	t.Helper()
	var next int64
	var count int
	if err := s.DB.QueryRow("SELECT next FROM desk_counter WHERE id=1").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM desk_numbers").Scan(&count); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Journal)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return next, count, string(data)
}
func TestAssignQueryCountIndependentOfCandidates(t *testing.T) {
	s, counts := countedFixture(t)
	all := candidates(50)
	assignOK(t, s, []List{{Project: "p", Issues: all}})
	var results [2]int
	for i, n := range []int{5, 50} {
		*counts = sqlCounts{enabled: true}
		assignOK(t, s, []List{{Project: "p", Issues: all[:n]}})
		counts.enabled = false
		results[i] = counts.selects
	}
	if results[0] == 0 || results[0] != results[1] {
		t.Fatalf("desk_numbers SELECT: %v", results)
	}
}
func TestAssignUnchangedDoesNotWrite(t *testing.T) {
	s, counts := countedFixture(t)
	lists := []List{{Project: "p", Issues: candidates(5)}}
	assignOK(t, s, lists)
	next, count, journal := snapshot(t, s)
	for i := 0; i < 2; i++ {
		*counts = sqlCounts{enabled: true}
		assignOK(t, s, lists)
		counts.enabled = false
		if counts.inserts != 0 || counts.updates != 0 || !s.checked {
			t.Fatalf("counts=%+v checked=%v", counts, s.checked)
		}
		n, c, j := snapshot(t, s)
		if n != next || c != count || j != journal {
			t.Fatal("state changed")
		}
	}
}
func TestAssignMixedCandidates(t *testing.T) {
	s, _ := countedFixture(t)
	initial := []List{{Project: "p", Issues: candidates(2)}}
	assignOK(t, s, initial)
	before, count, journal := snapshot(t, s)
	getNumber := func(issue int64) int64 {
		t.Helper()
		number, found, err := s.Number(context.Background(), "p", issue)
		if err != nil || !found || number == 0 {
			t.Fatalf("Number(p, %d) = %d, found=%v, err=%v", issue, number, found, err)
		}
		return number
	}
	a := getNumber(1)
	b := getNumber(2)
	assignOK(t, s, []List{{Project: "p", Issues: candidates(3)}})
	next, after, j := snapshot(t, s)
	n, found, err := s.Number(context.Background(), "p", 3)
	aa := getNumber(1)
	bb := getNumber(2)
	if err != nil || !found || n != before || next != before+1 || after != count+1 || strings.Count(j, "\n") != strings.Count(journal, "\n")+1 || a != aa || b != bb {
		t.Fatalf("new=%d next=%d count=%d err=%v", n, next, after, err)
	}
}
func TestAssignDuplicateCandidates(t *testing.T) {
	s, _ := countedFixture(t)
	assignOK(t, s, []List{{Project: "p"}})
	next, count, journal := snapshot(t, s)
	assignOK(t, s, []List{{Project: "p", Issues: []Candidate{{Number: 1}, {Number: 1}}}, {Project: "p", Issues: []Candidate{{Number: 1}}}})
	n, c, j := snapshot(t, s)
	number, found, err := s.Number(context.Background(), "p", 1)
	if err != nil || !found || number != next || n != next+1 || c != count+1 || strings.Count(j, "\n") != strings.Count(journal, "\n")+1 {
		t.Fatalf("number=%d next=%d count=%d err=%v", number, n, c, err)
	}
}
func TestAssignNoIssueStillPersistsRequiredChanges(t *testing.T) {
	s, _ := countedFixture(t)
	assignOK(t, s, []List{{Project: "p"}})
	assignOK(t, s, []List{{Project: "new"}})
	ready, err := s.Ready(context.Background(), []string{"new"})
	if err != nil || !ready {
		t.Fatalf("new project: %v %v", ready, err)
	}
	if _, err = s.DB.Exec("UPDATE desk_counter SET next=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(10,'p',1,'now')"); err != nil {
		t.Fatal(err)
	}
	assignOK(t, s, []List{{Project: "p", Issues: []Candidate{{Number: 1}}}})
	next, _, _ := snapshot(t, s)
	if next != 11 {
		t.Fatalf("next=%d", next)
	}
}
