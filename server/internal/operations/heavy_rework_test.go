package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/statistics"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestHeavyReworkAlertMatchesFullReportPostgres(t *testing.T) {
	db := postgres(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		var id int64
		if err := db.QueryRow(`INSERT INTO jobs(repo,issue,kind,pr,status,result,filed_at,tier,view) VALUES('owner/repo',$1,'followup',10,'merged','{"mergeCommit":"abc"}','2026-10-01','heavy','{"tierHistory":[{"from":"heavy","to":"heavy"}]}') RETURNING id`, i).Scan(&id); err != nil {
			t.Fatal(err)
		}
		row := fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"at":"2026-11-07T00:10:00Z","step":"実装","tier":"heavy","generation":1,"inputTokens":100,"outputTokens":10,"cacheReadTokens":0}`, i)
		if _, err := db.Exec(`INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES($1,'owner/repo',$2,'2026-11-07T00:10:00Z',$3,'nas','nas')`, fmt.Sprint(i), i, row); err != nil {
			t.Fatal(err)
		}
		if i <= 2 {
			if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'control','2026-11-07T00:30:00Z','{"action":"retry","fromBase":true}')`, id); err != nil {
				t.Fatal(err)
			}
		}
		for _, ev := range []struct{ kind, at, data string }{{"leased", "2026-11-07T00:00:00Z", `{"epoch":1,"generation":0}`}, {"state", "2026-11-07T01:00:00Z", `{"to":"merged","epoch":1}`}} {
			if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES($1,$2,$3,$4)`, id, ev.kind, ev.at, ev.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	stats := &statistics.Service{DB: db, Now: func() time.Time { return now }}
	s := &Service{Statistics: stats, Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	values := url.Values{"from": {"2026-10-26"}, "to": {"2026-11-08"}}
	full, err := stats.ReworkBreakdown(context.Background(), values, []string{"owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	r := fleet.Object(full)
	if r["implementation"] == nil || len(fleet.List(r["unavailable"])) != 0 {
		t.Fatal(full)
	}
	b := fleet.Object(r["breakdown"])
	expected := []any{}
	for _, raw := range fleet.List(b["tiers"]) {
		tier := fleet.Object(raw)
		if tier["key"] == "heavy" {
			expected = HeavyRework(tier, fleet.Object(b["period"]), fleet.List(r["unavailable"]))
		}
	}
	if len(expected) != 1 {
		t.Fatalf("fixture did not produce alert: %v", full)
	}
	got, err := s.heavyReworkAlerts(context.Background())
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("got %v expected %v error %v", got, expected, err)
	}
}

func TestHeavyReworkMissingStatisticsFails(t *testing.T) {
	s := &Service{}
	if _, err := s.heavyReworkAlerts(context.Background()); err == nil {
		t.Fatal("missing statistics accepted")
	}
	s.Statistics = &statistics.Service{}
	if _, err := s.heavyReworkAlerts(context.Background()); err == nil {
		t.Fatal("missing database accepted")
	}
}

func TestHeavyReworkEvidenceBoundariesPostgres(t *testing.T) {
	cases := []struct {
		name         string
		count        int
		event, extra string
		want         int
	}{
		{"two judged", 2, `{"action":"retry","fromBase":true}`, "", 0},
		{"three judged retry", 3, `{"action":"retry","fromBase":true}`, "", 1},
		{"exactly half", 4, `{"action":"retry","fromBase":true}`, "half", 0},
		{"retest", 3, `{"action":"retry","retest":true}`, "", 0},
		{"review hold", 3, `{"to":"held","holdReason":"review_held"}`, "", 1},
		{"review return", 3, `{"action":"noop"}`, "return", 1},
		{"unknown generation", 3, `{"action":"retry","fromBase":true}`, "unknown", 0},
		{"raised from light", 3, `{"action":"retry","fromBase":true}`, "raised", 0},
		{"after merge", 3, `{"action":"retry","fromBase":true}`, "after", 0},
		{"day before period", 3, `{"action":"retry","fromBase":true}`, "before-period", 0},
		{"first day of period", 3, `{"action":"retry","fromBase":true}`, "period-start", 1},
		{"day after period start", 3, `{"action":"retry","fromBase":true}`, "period-next", 1},
		{"missing usage", 3, `{"action":"retry","fromBase":true}`, "missing-usage", 0},
		{"invalid usage JSON", 3, `{"action":"retry","fromBase":true}`, "invalid-usage", 0},
		{"no heavy tier", 3, `{"action":"retry","fromBase":true}`, "no-heavy", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := postgres(t)
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			mergeAt := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
			switch tc.extra {
			case "before-period":
				mergeAt = time.Date(2026, 10, 25, 14, 59, 59, 0, time.UTC)
			case "period-start":
				mergeAt = time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC)
			case "period-next":
				mergeAt = time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
			}
			for i := 1; i <= tc.count; i++ {
				tier := "heavy"
				if tc.extra == "no-heavy" {
					tier = "light"
				}
				view := `{"tierHistory":[{"from":"heavy","to":"heavy"}]}`
				if tc.extra == "no-heavy" {
					view = `{"tierHistory":[{"from":"light","to":"light"}]}`
				}
				if tc.extra == "raised" {
					view = `{"tierHistory":[{"from":"light","to":"heavy"}]}`
				}
				var id int64
				if err := db.QueryRow(`INSERT INTO jobs(repo,issue,kind,pr,status,result,filed_at,tier,view) VALUES('owner/repo',$1,'followup',10,'merged','{"mergeCommit":"abc"}','2026-10-01',$3,$2) RETURNING id`, i, view, tier).Scan(&id); err != nil {
					t.Fatal(err)
				}
				gen := 1
				if tc.extra == "unknown" {
					gen = 0
				}
				usageAt := mergeAt.Add(-50 * time.Minute).Format(time.RFC3339)
				row := fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"at":%q,"step":"実装","tier":%q,"generation":%d,"inputTokens":100,"outputTokens":10,"cacheReadTokens":0}`, i, usageAt, tier, gen)
				if tc.extra == "invalid-usage" {
					row = "invalid JSON"
				}
				if tc.extra != "missing-usage" {
					if _, err := db.Exec(`INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES($1,'owner/repo',$2,$3,$4,'nas','nas')`, fmt.Sprint(i), i, usageAt, row); err != nil {
						t.Fatal(err)
					}
				}
				if tc.extra == "return" {
					row = fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"at":"2026-11-07T00:40:00Z","step":"実装","tier":"heavy","generation":2,"generationTrigger":"review-return","inputTokens":100,"outputTokens":10,"cacheReadTokens":0}`, i)
					if _, err := db.Exec(`INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES($1,'owner/repo',$2,'2026-11-07T00:40:00Z',$3,'nas','nas')`, fmt.Sprintf("return-%d", i), i, row); err != nil {
						t.Fatal(err)
					}
				}
				kind := "control"
				if tc.name == "review hold" {
					kind = "state"
				}
				if tc.extra == "half" && i > 2 {
					kind = "ignored"
				}
				at := mergeAt.Add(-30 * time.Minute).Format(time.RFC3339)
				if tc.extra == "after" {
					at = "2026-11-07T02:00:00Z"
				}
				if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES($1,$2,$3,$4),($1,'state',$5,'{"to":"merged"}')`, id, kind, at, tc.event, mergeAt); err != nil {
					t.Fatal(err)
				}
			}
			stats := &statistics.Service{DB: db, Now: func() time.Time { return now }}
			s := &Service{Statistics: stats, Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
			full, err := stats.ReworkBreakdown(context.Background(), url.Values{"from": {"2026-10-26"}, "to": {"2026-11-08"}}, []string{"owner/repo"})
			if err != nil {
				t.Fatal(err)
			}
			r := fleet.Object(full)
			b := fleet.Object(r["breakdown"])
			expected := []any{}
			for _, v := range fleet.List(b["tiers"]) {
				tier := fleet.Object(v)
				if tier["key"] == "heavy" {
					expected = HeavyRework(tier, fleet.Object(b["period"]), fleet.List(r["unavailable"]))
				}
			}
			got, err := s.heavyReworkAlerts(context.Background())
			if err != nil || !reflect.DeepEqual(got, expected) || len(got) != tc.want {
				t.Fatalf("got %v, full %v, want %d, err %v", got, expected, tc.want, err)
			}
		})
	}
}

// The list polls this alert every few seconds. Its result is kept for five
// minutes for the same period and repositories.
func TestHeavyReworkAlertIsKeptPostgres(t *testing.T) {
	db := postgres(t)
	// 23:53 JST: five minutes later is the same day, eight minutes later the next.
	start := time.Date(2026, 11, 8, 14, 53, 0, 0, time.UTC)
	now := start
	merged := func(issue int) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(`INSERT INTO jobs(repo,issue,kind,pr,status,result,filed_at,tier,view) VALUES('owner/repo',$1,'followup',10,'merged','{"mergeCommit":"abc"}','2026-10-01','heavy','{"tierHistory":[{"from":"heavy","to":"heavy"}]}') RETURNING id`, issue).Scan(&id); err != nil {
			t.Fatal(err)
		}
		row := fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"at":"2026-11-07T00:10:00Z","step":"実装","tier":"heavy","generation":1,"inputTokens":100,"outputTokens":10,"cacheReadTokens":0}`, issue)
		if _, err := db.Exec(`INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES($1,'owner/repo',$2,'2026-11-07T00:10:00Z',$3,'nas','nas')`, fmt.Sprint(issue), issue, row); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'leased','2026-11-07T00:00:00Z','{"epoch":1,"generation":0}'),($1,'state','2026-11-07T01:00:00Z','{"to":"merged","epoch":1}')`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	retried := func(id int64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'control','2026-11-07T00:30:00Z','{"action":"retry","fromBase":true}')`, id); err != nil {
			t.Fatal(err)
		}
	}
	retried(merged(1))
	retried(merged(2))
	merged(3)
	repos := []string{"owner/repo"}
	stats := &statistics.Service{DB: db, Now: func() time.Time { return now }}
	s := &Service{Statistics: stats, Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return repos, nil }}
	s.InitHeavyReworkCache()
	alerts := func(step string, count int) []any {
		t.Helper()
		got, err := s.heavyReworkAlerts(context.Background())
		if err != nil || len(got) != count {
			t.Fatalf("%s: got %v, error %v, expected %d alerts", step, got, err, count)
		}
		return got
	}
	first := alerts("two of three merges were retried", 1)
	want := fleet.Object(fleet.Clone(first[0]))

	// A fourth merge without a retry lowers the rate to the threshold.
	last := merged(4)
	now = start.Add(5*time.Minute - time.Second)
	kept := alerts("the kept alert ignores new merges", 1)
	// A caller changing what it received must not change what is kept.
	fleet.Object(kept[0])["title"] = "changed"
	fleet.Object(fleet.Object(kept[0])["period"])["fromDate"] = "changed"
	if got := alerts("the kept alert is returned again", 1); !reflect.DeepEqual(fleet.Clone(got[0]), any(want)) {
		t.Fatalf("the kept alert was changed by a caller: %v", got)
	}

	// Other repositories are another input: the alert is read again at once.
	repos = []string{"owner/repo", "owner/other"}
	alerts("changed repositories read again", 0)
	repos = []string{"owner/repo"}

	now = start.Add(5 * time.Minute)
	alerts("the alert is read again after five minutes", 0)

	// The fourth merge turns out retried: three of four is over the threshold.
	retried(last)
	now = start.Add(6 * time.Minute)
	alerts("the second result is kept", 0)
	// Past midnight the 14 days are another period, read again at once.
	now = start.Add(8 * time.Minute)
	alerts("a changed period reads again", 1)
}
