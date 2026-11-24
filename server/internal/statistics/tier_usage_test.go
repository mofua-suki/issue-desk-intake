package statistics

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"testing"
	"time"
)

func TestTierRatesExcludeUnknownAndKeepConsultationSharing(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	draft := []map[string]any{{"provider": "codex", "inputTokens": float64(10), "outputTokens": float64(2), "costUsd": nil}}
	work := []map[string]any{{"provider": "codex", "inputTokens": float64(20), "outputTokens": float64(3), "costUsd": nil, "step": "実装", "model": "model"}}
	combined := combinedOutcome(draft, work, 2)
	if fleet.Object(combined)["consultationShared"] != 2 || fleet.Object(combined)["inputTokens"] != float64(30) {
		t.Fatal(combined)
	}
	rows := []any{map[string]any{"createdAt": iso(at), "outcome": "passed", "combined": combined, "start": map[string]any{"level": "light"}, "model": "m", "issueUsage": outcomeIssue(work)}, map[string]any{"createdAt": iso(at), "outcome": nil, "combined": nil, "start": map[string]any{"level": "heavy"}}}
	report := tierRatePeriod(rows, Period{at, at.Add(time.Hour)})
	total := fleet.Object(report["total"])
	if total["judged"] != 1 || total["unknown"] != 1 || total["cheapPassed"] != 1 || total["passedRate"] != float64(.5) || total["cheapRate"] != float64(1) {
		t.Fatal(report)
	}
}
func TestScopeCheckDistinguishesBackfilledAndUnreadable(t *testing.T) {
	at := time.Now()
	lines := []logLine{{at, "範囲の照合 o/r#1（後から付けた宣言）: 宣言外の変更 2件（src/a）"}, {at, "範囲の照合 o/r#2: 実変更を読めません"}, {at, "範囲の照合 o/r#3: 宣言がありません（記録のみ）"}}
	out := scopeChecks(lines, Period{at.Add(-time.Second), at.Add(time.Second)}, "o/r", nil)
	if fleet.Object(out["backfilled"])["outside"] != 2 || fleet.Object(out["normal"])["unreadable"] != 1 || fleet.Object(out["undeclared"])["checks"] != 1 {
		t.Fatal(out)
	}
}

func TestTiersUsageIndexScaling(t *testing.T) {
	for _, size := range []struct{ drafts, rows int }{{800, 5000}, {8000, 50000}} {
		drafts := make([]map[string]any, size.drafts)
		for i := range drafts {
			drafts[i] = map[string]any{"id": fmt.Sprintf("d%d", i), "project": "o/r", "createdAt": "2026-01-01T00:00:00Z", "tier": map[string]any{"level": "light"}, "issue": map[string]any{"number": float64(i + 1)}, "messages": []any{map[string]any{"role": "assistant"}}}
		}
		rows := make([]map[string]any, size.rows)
		for i := range rows {
			switch i % 3 {
			case 0:
				rows[i] = map[string]any{"source": "intake", "draft": fmt.Sprintf("d%d", i%size.drafts), "provider": "codex", "inputTokens": float64(1)}
			case 1:
				rows[i] = map[string]any{"source": "worker", "repo": "o/r", "issue": float64(i%size.drafts + 1), "provider": "codex", "inputTokens": float64(1)}
			default:
				rows[i] = map[string]any{"source": "unrelated"}
			}
		}
		work := &statisticsWork{}
		ctx := context.WithValue(context.Background(), statisticsWorkKey{}, work)
		result, err := (&Service{}).calculateTiers(ctx, Query{}, drafts, nil, nil, rows)
		if err != nil {
			t.Fatal(err)
		}
		out := fleet.Object(result)
		if out["total"] != size.drafts || len(fleet.List(out["rows"])) != 200 {
			t.Fatal(out["total"], len(fleet.List(out["rows"])))
		}
		if work.tierRows < size.rows {
			t.Fatalf("usage rows were not counted: got %d, want at least %d", work.tierRows, size.rows)
		}
		// One examination per usage row, one lookup per draft and issue, and
		// at most one association per row in this one-issue-per-draft fixture.
		if limit := 3 * (size.drafts + size.rows); work.tierRows > limit {
			t.Fatalf("%+v: work=%d limit=%d", size, work.tierRows, limit)
		}
	}
}

func TestTiersIndexedResponseCompatibility(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	drafts := make([]map[string]any, 201)
	for i := range drafts {
		drafts[i] = map[string]any{"id": fmt.Sprintf("d%d", i), "project": "o/r", "createdAt": iso(at.Add(time.Duration(i) * time.Second)), "tier": map[string]any{"level": "light", "model": "m"}, "issue": map[string]any{"number": float64(i + 1)}, "messages": []any{}}
	}
	drafts[200]["issues"] = []any{map[string]any{"number": float64(201)}, map[string]any{"number": float64(202), "project": "other/r"}}
	drafts[200]["investigations"] = []any{map[string]any{"number": float64(203)}}
	rows := []map[string]any{
		{"source": "intake", "draft": "d200", "provider": "codex", "inputTokens": float64(10), "outputTokens": float64(1), "costUsd": float64(2)},
		{"source": "worker", "repo": "o/r", "issue": float64(201), "provider": "claude", "step": "実装", "generation": float64(1), "turn": float64(1), "costUsd": float64(3)},
		{"source": "worker", "repo": "o/r", "issue": float64(201), "provider": "claude", "step": "実装", "generation": float64(1), "turn": float64(2), "costUsd": float64(5)},
		{"source": "worker", "repo": "other/r", "issue": float64(202), "provider": "codex", "inputTokens": float64(7), "costUsd": nil},
		{"source": "intake", "draft": "unrelated", "costUsd": float64(999)},
	}
	period := Period{at, at.Add(24 * time.Hour)}
	v, err := (&Service{}).calculateTiers(context.Background(), Query{Before: period, After: period}, drafts, nil, nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	out := fleet.Object(v)
	shown := fleet.List(out["rows"])
	if out["total"] != 203 || out["omitted"] != 3 || len(shown) != 200 {
		t.Fatal(out["total"], out["omitted"], len(shown))
	}
	a, b, c := fleet.Object(shown[0]), fleet.Object(shown[1]), fleet.Object(shown[2])
	if a["draft"] != "d200" || fleet.Object(a["issue"])["number"] != int64(201) || fleet.Object(b["issue"])["number"] != int64(202) || fleet.Object(c["issue"])["investigation"] != true {
		t.Fatal(a, b, c)
	}
	if fleet.Object(a["usage"])["costUsd"] != float64(2) || fleet.Object(fleet.Object(a["issueUsage"])["total"])["costUsd"] != float64(5) || fleet.Object(a["combined"])["consultationShared"] != 3 || fleet.Object(b["combined"])["inputTokens"] != float64(17) || fleet.Object(c["issueUsage"]) != nil {
		t.Fatal(a, b, c)
	}
	if fleet.Object(fleet.Object(fleet.Object(out["rates"])["before"])["total"])["count"] != 203 {
		t.Fatal(out["rates"])
	}
}

func TestPostgresTiersIndexedUsageFromStoredRows(t *testing.T) {
	db, ctx := testPostgres(t)
	local := pgtest.New(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct{ key, source, raw string }{
		{"intake", "intake", `{"source":"intake","draft":"d1","provider":"codex","inputTokens":10,"outputTokens":1}`},
		{"worker", "worker", `{"source":"worker","repo":"o/r","issue":1,"provider":"codex","inputTokens":20,"outputTokens":2}`},
		{"other", "worker", `{"source":"worker","repo":"other/r","issue":1,"provider":"codex","inputTokens":999}`},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,json,sender) VALUES($1,$2,'nas',$3,'o/r',$4,'test')`, row.key, at.Add(-24*time.Hour), row.source, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	drafts := []map[string]any{{"id": "d1", "project": "o/r", "createdAt": iso(at), "tier": map[string]any{"level": "light"}, "issue": map[string]any{"number": float64(1)}, "messages": []any{}}}
	svc := &Service{DB: db, AppDB: local, Drafts: func(context.Context) ([]map[string]any, error) { return drafts, nil }}
	period := Period{at, at.Add(time.Hour)}
	value, err := svc.Tiers(ctx, Query{Before: period, After: period})
	if err != nil {
		t.Fatal(err)
	}
	rows := fleet.List(fleet.Object(value)["rows"])
	if len(rows) != 1 {
		t.Fatal(value)
	}
	row := fleet.Object(rows[0])
	combined := fleet.Object(row["combined"])
	if combined["inputTokens"] != float64(30) || combined["outputTokens"] != float64(3) || combined["consultationShared"] != 1 {
		t.Fatal(row)
	}
}

func TestTiersCountCodexConsultationCostsAsKnown(t *testing.T) {
	drafts := []map[string]any{{"id": "d1", "project": "o/r", "createdAt": "2026-11-13T00:00:00Z", "tier": map[string]any{"level": "heavy", "model": "gpt-6.1-sol"}, "issue": map[string]any{"number": float64(1)}, "messages": []any{}}}
	rows := []map[string]any{{"source": "intake", "draft": "d1", "provider": "codex", "model": "gpt-6.1-sol", "inputTokens": float64(100000), "cacheReadTokens": float64(0), "outputTokens": float64(1000), "contextMaxTokens": float64(100000), "costUsd": nil}}
	result, err := (&Service{}).calculateTiers(context.Background(), Query{}, drafts, nil, nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	groups := fleet.List(fleet.Object(result)["byLevelModel"])
	if len(groups) != 1 {
		t.Fatal(groups)
	}
	g := fleet.Object(groups[0])
	if g["costUnknown"] != 0 || fleet.Number(g["costUsd"]) < 0.2099 || fleet.Number(g["costUsd"]) > 0.2101 {
		t.Fatal(g)
	}
}
