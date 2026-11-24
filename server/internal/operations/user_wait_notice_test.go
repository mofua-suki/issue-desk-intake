package operations

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/jobs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func waitNoticeFixture(t *testing.T) (*UserWaitNotice, *[]UserWaitNoticeItem, *atomic.Int32) {
	t.Helper()
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	items := []UserWaitNoticeItem{{Project: "owner/repo", IssueNumber: 12, CommonNumber: 345, Title: "判断してください", Kind: jobs.UserWaitPermission}}
	calls := &atomic.Int32{}
	m := &UserWaitNotice{Service: s, Sources: func(context.Context) ([]UserWaitNoticeItem, error) { return items, nil }, Notify: func(context.Context, map[string]any) (map[string]any, error) { calls.Add(1); return nil, nil }}
	return m, &items, calls
}
func runWaitNotice(t *testing.T, m *UserWaitNotice) {
	t.Helper()
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestPostgresUserWaitNoticeOnce(t *testing.T) {
	m, items, calls := waitNoticeFixture(t)
	*items = append(*items, (*items)[0])
	runWaitNotice(t, m)
	(*items)[0].Title = "変更"
	runWaitNotice(t, m)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
func TestPostgresUserWaitNoticeRestart(t *testing.T) {
	m, _, calls := waitNoticeFixture(t)
	runWaitNotice(t, m)
	replacement := *m
	runWaitNotice(t, &replacement)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
func TestPostgresUserWaitNoticeReappears(t *testing.T) {
	m, items, calls := waitNoticeFixture(t)
	saved := (*items)[0]
	runWaitNotice(t, m)
	*items = nil
	runWaitNotice(t, m)
	*items = []UserWaitNoticeItem{saved}
	runWaitNotice(t, m)
	runWaitNotice(t, m)
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}
func TestPostgresUserWaitNoticeBatch(t *testing.T) {
	for _, change := range []string{"kind", "project"} {
		t.Run(change, func(t *testing.T) {
			m, items, _ := waitNoticeFixture(t)
			other := (*items)[0]
			if change == "kind" {
				other.Kind = jobs.UserWaitDeletion
			} else {
				other.Project = "other/repo"
			}
			*items = append(*items, other)
			calls := 0
			m.Notify = func(_ context.Context, msg map[string]any) (map[string]any, error) {
				calls++
				if !strings.Contains(fmt.Sprint(msg["title"]), "2件") || !strings.Contains(fmt.Sprint(msg["body"]), "345") || !strings.Contains(fmt.Sprint(msg["body"]), "判断してください") || !strings.Contains(fmt.Sprint(msg["body"]), "permission") || msg["url"] != "/#/list/attention" {
					t.Fatal(msg)
				}
				return nil, nil
			}
			runWaitNotice(t, m)
			if calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}
func TestPostgresUserWaitNoticeFailedOrUnsubscribed(t *testing.T) {
	for _, mode := range []string{"error", "failed", "unsubscribed"} {
		t.Run(mode, func(t *testing.T) {
			m, _, calls := waitNoticeFixture(t)
			m.Notify = func(context.Context, map[string]any) (map[string]any, error) {
				calls.Add(1)
				if mode == "error" {
					return nil, errors.New("send")
				}
				if mode == "failed" {
					return map[string]any{"failed": 1, "subscriptions": 1}, nil
				}
				return map[string]any{"subscriptions": 0}, nil
			}
			err := m.Reconcile(context.Background())
			if (err != nil) != (mode == "error") {
				t.Fatal(err)
			}
			runWaitNotice(t, m)
			replacement := *m
			runWaitNotice(t, &replacement)
			if calls.Load() != 1 {
				t.Fatal(calls.Load())
			}
		})
	}
}
func TestPostgresUserWaitNoticeSourceError(t *testing.T) {
	m, _, calls := waitNoticeFixture(t)
	runWaitNotice(t, m)
	before, err := m.Service.OperationalSetting(context.Background(), "user-wait-notice")
	if err != nil {
		t.Fatal(err)
	}
	m.Sources = func(context.Context) ([]UserWaitNoticeItem, error) { return nil, errors.New("source") }
	if m.Reconcile(context.Background()) == nil {
		t.Fatal("missing error")
	}
	after, err := m.Service.OperationalSetting(context.Background(), "user-wait-notice")
	if err != nil || fmt.Sprint(before) != fmt.Sprint(after) || calls.Load() != 1 {
		t.Fatal(before, after, err, calls.Load())
	}
}
func TestPostgresUserWaitNoticeReservation(t *testing.T) {
	m, _, calls := waitNoticeFixture(t)
	db := m.Service.PostgreSQL
	if _, err := db.Exec("INSERT INTO settings(key,value,updated_by) VALUES('incident-deadline','[1]'::jsonb,'test')"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- m.Reconcile(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	var incident string
	if err := db.QueryRow("SELECT value::text FROM settings WHERE key='incident-deadline'").Scan(&incident); err != nil || incident != "[1]" {
		t.Fatal(incident, err)
	}
	if _, err := db.Exec("UPDATE settings SET value='[{}]'::jsonb WHERE key='user-wait-notice'"); err != nil {
		t.Fatal(err)
	}
	if m.Reconcile(context.Background()) == nil || calls.Load() != 1 {
		t.Fatal("invalid ledger accepted")
	}
	if _, err := db.Exec("DROP TABLE settings"); err != nil {
		t.Fatal(err)
	}
	if m.Reconcile(context.Background()) == nil || calls.Load() != 1 {
		t.Fatal("storage failure accepted")
	}
}
