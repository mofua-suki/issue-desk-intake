package statistics

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"reflect"
	"testing"
	"time"
)

func TestUsageReviewPeriodsAndUnknownCounts(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	round := map[string]any{}
	result, _, before, _ := reviewRange(round, now)
	if result != "cost_only" || before != nil {
		t.Fatal(result, before)
	}
	round["treatmentIssues"] = []any{map[string]any{"project": "o/r", "number": float64(1)}}
	result, _, _, _ = reviewRange(round, now)
	if result != "waiting" {
		t.Fatal(result)
	}
	round["treatmentIssues"] = []any{map[string]any{"mergedAt": iso(now.Add(-2 * 24 * time.Hour))}}
	result, _, before, after := reviewRange(round, now)
	if result != "waiting" || before.To != after.From || after.To != now {
		t.Fatal(result, before, after)
	}
	round["treatmentIssues"] = []any{map[string]any{"mergedAt": iso(now.Add(-4 * 24 * time.Hour))}}
	rows := []map[string]any{{"provider": "claude", "costUsd": float64(2)}, {"provider": "claude", "costUsd": nil}, {"provider": "codex", "inputTokens": nil}}
	tally := tallyReview(rows, nil)
	if fleet.Object(tally["claude"])["value"] != float64(2) || fleet.Object(tally["claude"])["unknown"] != 1 || fleet.Object(tally["codex"])["value"] != nil {
		t.Fatal(tally)
	}
	cost := tallyReview(nil, nil)
	_, verdict, _ := reviewJudge(round, nil, cost, nil, now, time.Time{})
	if verdict["result"] != "undeterminable" {
		t.Fatal(verdict)
	}
	_, verdict, _ = reviewJudge(round, nil, cost, nil, now, now.Add(-12*24*time.Hour))
	if verdict["result"] != "recouped" {
		t.Fatal(verdict)
	}
}

func TestUsageReviewUnacknowledgedMatchesFullReport(t *testing.T) {
	db, ctx := testPostgres(t)
	local := pgtest.New(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		round int
		draft string
	}{{1, "one"}, {2, "two"}, {3, "three"}} {
		raw, _ := json.Marshal(map[string]any{"round": item.round, "draftId": item.draft, "startedAt": "2026-10-01T15:00:00Z", "treatmentIssues": []any{map[string]any{"mergedAt": "2026-10-27T00:00:00Z"}}})
		if _, err := local.ExecContext(ctx, `INSERT INTO usage_review_rounds(round,json) VALUES($1,$2)`, item.round, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := local.ExecContext(ctx, `INSERT INTO usage_review_acks(round,at) VALUES(1,'2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for i, row := range []map[string]any{
		{"at": "2026-10-15T15:00:00Z", "provider": "claude", "costUsd": float64(1)},
		{"at": "2026-10-24T00:00:00Z", "provider": "claude", "costUsd": float64(10), "repo": "o/r", "issue": float64(7), "generation": float64(1), "turn": float64(1), "step": "実装"},
		{"at": "2026-10-25T00:00:00Z", "provider": "claude", "costUsd": float64(12), "repo": "o/r", "issue": float64(7), "generation": float64(1), "turn": float64(2), "step": "実装"},
		{"at": "2026-10-16T00:00:00Z", "provider": "claude", "costUsd": nil, "source": "intake", "draft": "two"},
		{"at": "2026-10-29T00:00:00Z", "provider": "claude", "costUsd": float64(20), "source": "intake", "draft": "three"},
	} {
		raw, _ := json.Marshal(row)
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,json,sender) VALUES($1,$2,'nas','intake','o/r',$3,'nas')`, fmt.Sprint(i), row["at"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	s := Service{DB: db, AppDB: local, Now: func() time.Time { return now }}
	full, err := s.UsageReview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := s.UsageReviewUnacknowledged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all := fleet.List(fleet.Object(full)["rounds"])
	pending := fleet.List(fleet.Object(filtered)["rounds"])
	if len(all) != 3 || len(pending) != 2 || !reflect.DeepEqual(all[1:], pending) {
		t.Fatalf("full=%v pending=%v", all, pending)
	}
	if fleet.Object(fleet.Object(pending[0])["verdict"])["result"] != "undeterminable" || fleet.Object(fleet.Object(pending[1])["verdict"])["result"] != "not_recouped" {
		t.Fatalf("unexpected verdicts: %v", pending)
	}
	if _, err = local.ExecContext(ctx, `INSERT INTO usage_review_acks(round,at) VALUES(2,'yes'),(3,'yes')`); err != nil {
		t.Fatal(err)
	}
	// A nil usage DB proves that the all-acknowledged path performs no usage query.
	s.DB = nil
	filtered, err = s.UsageReviewUnacknowledged(ctx)
	if err != nil || len(fleet.List(fleet.Object(filtered)["rounds"])) != 0 {
		t.Fatalf("%v %v", filtered, err)
	}
}

// BenchmarkUsageReviewComparison measures the per-round comparison on the same
// synthetic history; the production usage query remains one for mixed rounds.
func BenchmarkUsageReviewComparison(b *testing.B) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]map[string]any, 1000)
	for i := range rows {
		rows[i] = map[string]any{"at": iso(now.Add(-time.Duration(i%300) * time.Hour)), "provider": "claude", "costUsd": float64(1), "repo": "o/r", "issue": float64(i + 1)}
	}
	round := map[string]any{"treatmentIssues": []any{map[string]any{"mergedAt": iso(now.Add(-4 * 24 * time.Hour))}}}
	for _, count := range []int{100, 10} {
		b.Run(fmt.Sprintf("rounds_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j := 0; j < count; j++ {
					cost := tallyReview(nil, nil)
					reviewJudge(round, rows, cost, nil, now, now.Add(-30*24*time.Hour))
				}
			}
		})
	}
}

// The list polls the notifications every few seconds. Their judgments are kept
// for five minutes, but acknowledgements and changed rounds show at once.
func TestUsageReviewNotificationsKeepJudgments(t *testing.T) {
	db, ctx := testPostgres(t)
	local := pgtest.New(t)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := start
	round := func(number int, draft string, extra map[string]any) string {
		record := map[string]any{"round": number, "draftId": draft, "startedAt": "2026-10-01T15:00:00Z", "treatmentIssues": []any{map[string]any{"mergedAt": "2026-10-27T00:00:00Z"}}}
		for k, v := range extra {
			record[k] = v
		}
		raw, _ := json.Marshal(record)
		return string(raw)
	}
	for number, draft := range map[int]string{2: "two", 3: "three"} {
		if _, err := local.ExecContext(ctx, `INSERT INTO usage_review_rounds(round,json) VALUES($1,$2)`, number, round(number, draft, nil)); err != nil {
			t.Fatal(err)
		}
	}
	usageRows := 0
	addUsage := func(row map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(row)
		usageRows++
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,json,sender) VALUES($1,$2,'nas','intake','o/r',$3,'nas')`, fmt.Sprint(usageRows), row["at"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	addUsage(map[string]any{"at": "2026-10-15T15:00:00Z", "provider": "claude", "costUsd": float64(1)})
	addUsage(map[string]any{"at": "2026-10-24T00:00:00Z", "provider": "claude", "costUsd": float64(12)})
	addUsage(map[string]any{"at": "2026-10-29T00:00:00Z", "provider": "claude", "costUsd": float64(20), "source": "intake", "draft": "three"})
	s := Service{DB: db, AppDB: local, Now: func() time.Time { return now }}
	s.InitUsageReviewCache()
	// judged is a round's review cost, the usage after its treatment and the verdict.
	type judged struct {
		cost, after any
		result      any
	}
	read := func(report any, err error) map[float64]judged {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := map[float64]judged{}
		for _, raw := range fleet.List(fleet.Object(report)["rounds"]) {
			r := fleet.Object(raw)
			after := fleet.Object(fleet.Object(fleet.Object(fleet.Object(r["comparison"])["providers"])["claude"])["after"])
			out[fleet.Number(r["round"])] = judged{fleet.Object(fleet.Object(r["cost"])["claude"])["value"], after["value"], fleet.Object(r["verdict"])["result"]}
		}
		return out
	}
	pending := func() map[float64]judged { t.Helper(); return read(s.UsageReviewUnacknowledged(ctx)) }
	want := func(step string, got, expected map[float64]judged) {
		t.Helper()
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s: got %v, expected %v", step, got, expected)
		}
	}
	first := map[float64]judged{2: {float64(0), float64(20), "not_recouped"}, 3: {float64(20), float64(0), "not_recouped"}}
	want("first read", pending(), first)

	// New usage: 5 more for the review of round 3, which round 2 counts as usage after its treatment.
	addUsage(map[string]any{"at": "2026-10-30T00:00:00Z", "provider": "claude", "costUsd": float64(5), "source": "intake", "draft": "three"})
	now = start.Add(5*time.Minute - time.Second)
	want("kept judgments ignore new usage", pending(), first)
	current := map[float64]judged{2: {float64(0), float64(25), "not_recouped"}, 3: {float64(25), float64(0), "not_recouped"}}
	want("the full report is always current", read(s.UsageReview(ctx)), current)

	// A caller changing what it received must not change what is kept.
	report, err := s.UsageReviewUnacknowledged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range fleet.List(fleet.Object(report)["rounds"]) {
		r := fleet.Object(raw)
		fleet.Object(r["verdict"])["result"] = "changed"
		fleet.Object(fleet.Object(r["cost"])["claude"])["value"] = float64(999)
		fleet.Object(fleet.Object(fleet.Object(fleet.Object(r["comparison"])["providers"])["claude"])["after"])["value"] = float64(999)
	}
	want("kept judgments are not changed by a caller", pending(), first)

	// A changed round is judged again at once; the other round stays kept.
	if _, err := local.ExecContext(ctx, `UPDATE usage_review_rounds SET json=$1 WHERE round=2`, round(2, "two", map[string]any{"noAction": true})); err != nil {
		t.Fatal(err)
	}
	want("a changed round is judged again", pending(), map[float64]judged{2: current[2], 3: first[3]})

	now = start.Add(5 * time.Minute)
	want("judgments are read again after five minutes", pending(), current)

	// Changed weights are a changed input, too.
	addUsage(map[string]any{"at": "2026-10-30T01:00:00Z", "provider": "claude", "costUsd": float64(1), "source": "intake", "draft": "three"})
	now = now.Add(time.Second)
	want("the second judgments are kept", pending(), current)
	s.Weights = map[string]any{"codex": map[string]any{"inputTokens": float64(2)}}
	latest := map[float64]judged{2: {float64(0), float64(26), "not_recouped"}, 3: {float64(26), float64(0), "not_recouped"}}
	want("changed weights are judged again", pending(), latest)

	// An acknowledgement hides the round at once, while the judgments are kept.
	if _, err := local.ExecContext(ctx, `INSERT INTO usage_review_acks(round,at) VALUES(3,'2026-11-01T00:05:01Z')`); err != nil {
		t.Fatal(err)
	}
	want("an acknowledged round is hidden at once", pending(), map[float64]judged{2: latest[2]})
}
