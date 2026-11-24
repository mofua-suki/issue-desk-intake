package filing

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"sync"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
)

func finishService(t *testing.T, s *Service) *consultations.Service {
	t.Helper()
	c, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFinishWinsBeforeAutomaticFiling(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "split"}[split], func(t *testing.T) {
			s, api := fixture(t)
			c := finishService(t, s)
			ctx := context.Background()
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
				d["status"] = "auto_ready"
				if split {
					d["splitIssues"] = []any{Record{}, Record{}}
				}
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			if split {
				s.options.SplitPlan = func(Record) ([]Record, error) {
					close(entered)
					<-release
					return []Record{{"project": "owner/app", "title": "one"}, {"project": "owner/app", "title": "two"}}, nil
				}
			} else {
				s.options.Acquire = func(ctx context.Context, _ string) (context.Context, func(), error) {
					close(entered)
					<-release
					return ctx, func() {}, nil
				}
			}
			done := make(chan error, 1)
			go func() {
				_, e := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
				done <- e
			}()
			<-entered
			_, err = c.Finish(ctx, draftID, consultations.Record{}, auth.Principal{Kind: auth.Screen})
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			if err = <-done; err == nil {
				t.Fatal("filing succeeded after finish")
			}
			d, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil || d["status"] != "submitted" || d["finishedWithoutIssue"] != true || api.posts != 0 {
				t.Fatalf("draft=%v posts=%d err=%v", d, api.posts, err)
			}
			var count int
			if err = s.options.Records.DB.QueryRowContext(ctx, "SELECT count(*) FROM drafts").Scan(&count); err != nil || count != 1 {
				t.Fatalf("draft count=%d err=%v", count, err)
			}
		})
	}
}

func assertFilingWon(t *testing.T, s *Service, api *memoryAPI, split bool) {
	t.Helper()
	d, exists, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil || !exists || d["status"] != "submitted" || d["finishedWithoutIssue"] != nil {
		t.Fatalf("filing result: draft=%v exists=%v err=%v", d, exists, err)
	}
	if split {
		if api.posts != 2 || len(fleet.Items(d["issues"])) != 2 || len(fleet.Items(d["splitChildren"])) != 2 || d["issue"] != nil {
			t.Fatalf("split filing result: draft=%v posts=%d", d, api.posts)
		}
	} else if api.posts != 1 || d["issue"] == nil {
		t.Fatalf("normal filing result: draft=%v posts=%d", d, api.posts)
	}
}

func TestFinishConcurrentWithFiling(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "split"}[split], func(t *testing.T) {
			s, api := fixture(t)
			c := finishService(t, s)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
				d["status"] = "auto_ready"
				if split {
					d["splitIssues"] = []any{Record{}, Record{}}
				}
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if split {
				s.options.SplitPlan = func(Record) ([]Record, error) {
					return []Record{{"project": "owner/app", "title": "one"}, {"project": "owner/app", "title": "two"}}, nil
				}
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var finishErr, filingErr error
			go func() { defer wg.Done(); <-start; _, finishErr = c.Finish(ctx, draftID, consultations.Record{}, p) }()
			go func() { defer wg.Done(); <-start; _, filingErr = s.Submit(ctx, draftID, Record{"revision": 0}, p) }()
			close(start)
			wg.Wait()
			d, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			if finishErr == nil {
				var count int
				if err = s.options.Records.DB.QueryRowContext(ctx, "SELECT count(*) FROM drafts").Scan(&count); err != nil || filingErr == nil || api.posts != 0 || d["finishedWithoutIssue"] != true || count != 1 {
					t.Fatalf("finish=%v filing=%v draft=%v posts=%d count=%d err=%v", finishErr, filingErr, d, api.posts, count, err)
				}
			} else {
				if filingErr != nil {
					t.Fatalf("filing did not win: %v %v %v posts=%d", finishErr, filingErr, d, api.posts)
				}
				assertFilingWon(t, s, api, split)
			}
		})
	}
}

type pausedHistoryAPI struct {
	*memoryAPI
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *pausedHistoryAPI) Issues(ctx context.Context, repo, state string) ([]map[string]any, error) {
	a.once.Do(func() { close(a.entered); <-a.release })
	return a.memoryAPI.Issues(ctx, repo, state)
}
func TestFilingWinsBeforeFinish(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "split"}[split], func(t *testing.T) {
			s, api := fixture(t)
			c := finishService(t, s)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if split {
				_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["splitIssues"] = []any{Record{}, Record{}}; return d, nil })
				if err != nil {
					t.Fatal(err)
				}
				s.options.SplitPlan = func(Record) ([]Record, error) {
					return []Record{{"project": "owner/app", "title": "one"}, {"project": "owner/app", "title": "two"}}, nil
				}
			}
			gate := &pausedHistoryAPI{memoryAPI: api, entered: make(chan struct{}), release: make(chan struct{})}
			s.options.API = gate
			done := make(chan error, 1)
			go func() { _, err := s.Submit(ctx, draftID, Record{"revision": 0}, p); done <- err }()
			<-gate.entered
			_, err := c.Finish(ctx, draftID, consultations.Record{}, p)
			if err == nil {
				close(gate.release)
				t.Fatal("finish accepted while submitting")
			}
			if !strings.Contains(err.Error(), "GET /api/drafts/") {
				close(gate.release)
				t.Fatalf("missing guidance: %v", err)
			}
			d, _, readErr := s.options.Records.Get(ctx, "drafts", draftID)
			if readErr != nil || d["status"] != "submitting" || d["finishedWithoutIssue"] != nil || split && len(fleet.Items(d["splitChildren"])) != 2 {
				close(gate.release)
				t.Fatalf("draft=%v err=%v", d, readErr)
			}
			close(gate.release)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			assertFilingWon(t, s, api, split)
		})
	}
}
