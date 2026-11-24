package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// issueListJobs reads through the cache and requires the result to equal a
// read that decodes every row.
func issueListJobs(t *testing.T, ctx context.Context, s *Store) []*Job {
	t.Helper()
	got, err := s.AllForIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readAllJobs(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("cached read has %d jobs, full read has %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("cached job differs from the row:\ncached=%+v\nrow=%+v", got[i], want[i])
		}
	}
	return got
}

func jobByID(jobs []*Job, id int64) *Job {
	for _, job := range jobs {
		if job.ID == id {
			return job
		}
	}
	return nil
}

func TestIssueListCacheFollowsRowChangesPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	a := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	b := seedJob(t, ctx, db, "owner/repo", "nas", 2, 1)
	c := seedJob(t, ctx, db, "owner/other", "nas", 1, 1)
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET result='{"status":"failed","review":{"findings":["x"]}}', scope=ARRAY['src/','docs/'] WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := issueListJobs(t, ctx, s)
	if len(first) != 3 || len(s.issueList.entries) != 3 {
		t.Fatalf("first read: %d jobs, %d cached", len(first), len(s.issueList.entries))
	}
	// reread returns the rows decoded since the previous call of reread.
	reread := func() int {
		n := s.issueList.metric.reread
		s.issueList.metric.reread = 0
		return n
	}
	reread()

	// Unchanged rows are not decoded again: the same jobs come back.
	second := issueListJobs(t, ctx, s)
	for i := range first {
		if second[i] != first[i] {
			t.Fatalf("job %d was decoded again without a change", first[i].ID)
		}
	}
	if n := reread(); n != 0 {
		t.Fatalf("unchanged read decoded %d rows", n)
	}

	// An update replaces only the updated job.
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET title='changed', view=view||'{"phase":"publishing"}', result='{"status":"succeeded"}' WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	third := issueListJobs(t, ctx, s)
	updated := jobByID(third, b)
	if updated == jobByID(first, b) || updated.Title != "changed" || updated.View["phase"] != "publishing" || updated.Result["status"] != "succeeded" {
		t.Fatalf("update is not reflected: %+v", updated)
	}
	if jobByID(third, a) != jobByID(first, a) || jobByID(third, c) != jobByID(first, c) {
		t.Fatal("an update of one row decoded other rows again")
	}
	if n := reread(); n != 1 {
		t.Fatalf("one update decoded %d rows", n)
	}

	// A rolled-back update leaves the cached job in place.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET title='discarded' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if jobByID(issueListJobs(t, ctx, s), a).Title == "discarded" {
		t.Fatal("a rolled-back update reached the list")
	}

	// An inserted row appears, a deleted row disappears from the result and the cache.
	d := seedJob(t, ctx, db, "owner/repo", "nas", 3, 1)
	if _, err = db.ExecContext(ctx, `DELETE FROM jobs WHERE id=$1`, c); err != nil {
		t.Fatal(err)
	}
	fourth := issueListJobs(t, ctx, s)
	if len(fourth) != 3 || jobByID(fourth, d) == nil || jobByID(fourth, c) != nil {
		t.Fatalf("insert and delete are not reflected: %d jobs", len(fourth))
	}
	if _, cached := s.issueList.entries[c]; cached || len(s.issueList.entries) != 3 {
		t.Fatalf("deleted job stays cached: %d entries", len(s.issueList.entries))
	}
	if n := reread(); n != 1 {
		t.Fatalf("one insert decoded %d rows", n)
	}

	// Deleting a row and inserting the same issue again yields the new row.
	if _, err = db.ExecContext(ctx, `DELETE FROM jobs WHERE id=$1`, d); err != nil {
		t.Fatal(err)
	}
	again := seedJob(t, ctx, db, "owner/repo", "nas", 3, 2)
	fifth := issueListJobs(t, ctx, s)
	if job := jobByID(fifth, again); again == d || job == nil || job.LeaseEpoch != 2 || jobByID(fifth, d) != nil {
		t.Fatalf("re-created job is not reflected: %+v", job)
	}

	// With every row gone the list is empty.
	if _, err = db.ExecContext(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if rest := issueListJobs(t, ctx, s); len(rest) != 0 || len(s.issueList.entries) != 0 {
		t.Fatalf("jobs remain after deleting all: %d", len(rest))
	}
}

// A failed read keeps the cache, and the next read still follows the table.
func TestIssueListCacheRecoversAfterFailurePostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	a := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	issueListJobs(t, ctx, s)
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET title='after' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.AllForIssues(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	if job := jobByID(issueListJobs(t, ctx, s), a); job.Title != "after" {
		t.Fatalf("read after a failure: %+v", job)
	}
	// A store that New did not make has no cache and decodes every row.
	plain, err := (&Store{db: db}).AllForIssues(ctx)
	if err != nil || len(plain) != 1 || plain[0].Title != "after" {
		t.Fatalf("store without a cache: %v %v", plain, err)
	}
}

// Two rows written in one transaction are never seen half-updated. Between the
// paired writes only the first row changes, so a read often has that row to
// decode again while the second row is cached: taking the versions and the rows
// from different snapshots would pair a new first row with an old second row.
func TestIssueListCacheReadsOneSnapshotPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	a := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	b := seedJob(t, ctx, db, "owner/repo", "nas", 2, 1)
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET title='0'`); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const writes = 300
	done := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		defer close(done)
		for i := 1; i <= writes; i++ {
			err := func() error {
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET priority=priority WHERE id=$1`, a); err != nil {
					return err
				}
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				for _, id := range []int64{a, b} {
					if _, err = tx.ExecContext(ctx, `UPDATE jobs SET title=$1 WHERE id=$2`, strconv.Itoa(i), id); err != nil {
						return err
					}
				}
				return tx.Commit()
			}()
			if err != nil {
				writeErr <- err
				return
			}
		}
	}()
	var readers sync.WaitGroup
	failures := make(chan string, 4)
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				all, err := s.AllForIssues(ctx)
				if err != nil {
					failures <- err.Error()
					return
				}
				if first, second := jobByID(all, a), jobByID(all, b); first == nil || second == nil || first.Title != second.Title {
					failures <- "the two rows of one transaction differ"
					return
				}
			}
		}()
	}
	readers.Wait()
	<-done
	select {
	case err := <-writeErr:
		t.Fatal(err)
	case message := <-failures:
		t.Fatal(message)
	default:
	}
	if job := jobByID(issueListJobs(t, ctx, s), a); job.Title != strconv.Itoa(writes) {
		t.Fatalf("last write is not reflected: %q", job.Title)
	}
}

func TestIssueListCacheObservationPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	a := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	seedJob(t, ctx, db, "owner/repo", "nas", 2, 1)
	at := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	events := func() []map[string]any {
		t.Helper()
		out := []map[string]any{}
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if line == "" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			if event["msg"] == "job list cache observation" {
				out = append(out, event)
			}
		}
		logs.Reset()
		return out
	}
	var size int
	if err = db.QueryRowContext(ctx, `SELECT sum(octet_length(repo)+octet_length(title)+octet_length(to_json(scope)::text)+octet_length(view::text)+octet_length(coalesce(result::text,'')))::int FROM jobs`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	issueListJobs(t, ctx, s)
	if got := events(); len(got) != 1 || got[0]["status"] != "success" || got[0]["count"] != float64(2) || got[0]["logical_bytes"] != float64(size) || got[0]["reads"] != float64(1) || got[0]["reread_rows"] != float64(2) || got[0]["measured_at"] == nil {
		t.Fatalf("first read: %v (bytes want %d)", got, size)
	}
	// Reads inside the interval stay quiet; the next event carries their totals.
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET title='changed' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	issueListJobs(t, ctx, s)
	issueListJobs(t, ctx, s)
	if got := events(); len(got) != 0 {
		t.Fatalf("reads inside the interval logged: %v", got)
	}
	at = at.Add(issueListMetricInterval)
	issueListJobs(t, ctx, s)
	if got := events(); len(got) != 1 || got[0]["reads"] != float64(3) || got[0]["reread_rows"] != float64(1) || got[0]["count"] != float64(2) {
		t.Fatalf("interval event: %v", got)
	}
	// Repeated failures log once, and the recovery logs at once.
	working := s.db
	s.db = closedJobsDB(t)
	for range 2 {
		if _, err = s.AllForIssues(ctx); err == nil {
			t.Fatal("read from a closed database succeeded")
		}
	}
	s.db = working
	if got := events(); len(got) != 1 || got[0]["status"] != "missing" || got[0]["count"] != nil {
		t.Fatalf("failure events: %v", got)
	}
	issueListJobs(t, ctx, s)
	if got := events(); len(got) != 1 || got[0]["status"] != "success" || got[0]["count"] != float64(2) {
		t.Fatalf("recovery event: %v", got)
	}
}

// closedJobsDB returns a closed connection pool: every read on it fails.
func closedJobsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := testPostgres(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return db
}
