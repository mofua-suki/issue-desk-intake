package screenapi

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"reflect"
	"testing"
	"time"
)

// referenceWithdrawalUsage is the original per-operation scan, retained as an oracle.
func referenceWithdrawalUsage(records, rows []Object, weights map[string]any) error {
	for _, r := range records {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil {
			return fail(503, "操作時刻を確認できません。")
		}
		used := Object{"rows": 0, "weighted": nil, "missing": 0}
		for _, u := range rows {
			if u["source"] != "intake" || u["draft"] != r["draft"] {
				continue
			}
			atUsage, e := time.Parse(time.RFC3339Nano, fleet.Text(u["at"]))
			if e != nil || atUsage.After(at) {
				continue
			}
			used["rows"] = intValue(used["rows"]) + 1
			if v, ok := usage.Weighted(u, weights)["weighted"].(float64); ok {
				previous, _ := used["weighted"].(float64)
				used["weighted"] = previous + v
			} else {
				used["missing"] = intValue(used["missing"]) + 1
			}
		}
		r["used"] = used
	}
	return nil
}

func withdrawalFixture(unrelated, operations int) ([]Object, []Object) {
	records := make([]Object, operations)
	for i := range records {
		records[i] = Object{"draft": "target", "at": fmt.Sprintf("2026-10-31T%02d:00:00Z", i%24)}
	}
	rows := make([]Object, 0, unrelated+6)
	for i := 0; i < unrelated; i++ {
		rows = append(rows, Object{"source": "intake", "draft": fmt.Sprintf("other-%d", i), "at": "2026-10-01T00:00:00Z"})
	}
	for _, item := range []struct {
		at    string
		value float64
	}{{"2026-09-01T00:00:00Z", 1e16}, {"2026-10-31T12:00:00Z", 1}, {"2026-10-31T12:00:00Z", 1}, {"2026-10-31T01:00:00Z", -1e16}} {
		rows = append(rows, Object{"source": "intake", "draft": "target", "at": item.at, "provider": "claude", "inputTokens": item.value, "outputTokens": float64(0), "cacheReadTokens": float64(0), "cacheWriteTokens": float64(0)})
	}
	rows = append(rows, Object{"source": "intake", "draft": "target", "at": "2026-10-31T01:00:00Z"}, Object{"source": "intake", "draft": "target", "at": "invalid"}, Object{"source": "other", "draft": "target", "at": "2026-10-01T00:00:00Z"})
	return records, rows
}
func copyWithdrawalRecords(rows []Object) []Object {
	out := make([]Object, len(rows))
	for i, row := range rows {
		out[i] = Object{"draft": row["draft"], "at": row["at"]}
	}
	return out
}

func TestWithdrawalUsageMatchesOriginal(t *testing.T) {
	records, rows := withdrawalFixture(10000, 48)
	records = append(records, Object{"draft": "missing", "at": "2026-10-31T12:00:00Z"}, Object{"draft": "target", "at": "2026-10-31T12:00:00Z"})
	want, got := copyWithdrawalRecords(records), copyWithdrawalRecords(records)
	if err := referenceWithdrawalUsage(want, rows, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyWithdrawalUsage(got, rows, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	bad := []Object{{"draft": "target", "at": "invalid"}}
	if referenceWithdrawalUsage(copyWithdrawalRecords(bad), rows, nil) == nil || applyWithdrawalUsage(copyWithdrawalRecords(bad), rows, nil, nil) == nil {
		t.Fatal("invalid operation accepted")
	}
}

func TestWithdrawalUsageFullInputVisits(t *testing.T) {
	for _, operations := range []int{1, 100} {
		records, rows := withdrawalFixture(20000, operations)
		visits := 0
		if err := applyWithdrawalUsage(records, rows, nil, &visits); err != nil {
			t.Fatal(err)
		}
		if visits != len(rows) {
			t.Fatalf("%d operations: visited %d full input rows, want %d", operations, visits, len(rows))
		}
	}
}

func TestWithdrawalUsageAPIContracts(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.s.o.Usage = func(_ context.Context, from, to time.Time) ([]Object, error) {
		calls++
		if !from.Equal(time.Unix(0, 0)) {
			t.Fatal(from)
		}
		return nil, errors.New("failed")
	}
	code, v := f.call("GET", "/api/draft-withdrawals?from=2026-11-01&to=2026-11-01", "token", nil)
	if code != 200 || fleet.ObjectOrEmpty(v)["reason"] != "相談の使用量を取得できません。" || len(fleet.Items(fleet.ObjectOrEmpty(v)["records"])) != 0 || calls != 1 {
		t.Fatal(code, v, calls)
	}
	f.s.o.Usage = nil
	code, v = f.call("GET", "/api/draft-withdrawals?from=2026-11-01&to=2026-11-01", "token", nil)
	if code != 200 || fleet.ObjectOrEmpty(v)["reason"] != "相談の使用量を取得する経路が設定されていません。" {
		t.Fatal(code, v)
	}
	for i, at := range []string{"2026-10-31T10:00:00.000Z", "2026-10-31T12:00:00.000Z", "2026-10-31T12:00:00.000Z"} {
		_, err := f.db.DB().Exec(`INSERT INTO draft_withdrawals(sequence,draft,project,operation,at,via) VALUES(?,?,'owner/repo','todo',?,'token')`, i+1, fixtureID, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.s.o.Usage = func(context.Context, time.Time, time.Time) ([]Object, error) { return nil, errors.New("failed") }
	code, v = f.call("GET", "/api/draft-withdrawals?from=2026-10-31&to=2026-10-31", "token", nil)
	if code != 200 || fleet.ObjectOrEmpty(v)["reason"] == nil || len(fleet.Items(fleet.ObjectOrEmpty(v)["records"])) != 3 || fleet.ObjectOrEmpty(fleet.Items(fleet.ObjectOrEmpty(v)["records"])[0])["used"] != nil {
		t.Fatal(code, v)
	}
	_, usageRows := withdrawalFixture(10, 1)
	for _, row := range usageRows {
		if row["draft"] == "target" {
			row["draft"] = fixtureID
		}
	}
	f.s.o.Usage = func(context.Context, time.Time, time.Time) ([]Object, error) { return usageRows, nil }
	code, v = f.call("GET", "/api/draft-withdrawals?from=2026-10-31&to=2026-10-31&project=owner%2Frepo", "token", nil)
	if code != 200 || fleet.ObjectOrEmpty(v)["reason"] != nil {
		t.Fatal(code, v)
	}
	actual := fleet.Items(fleet.ObjectOrEmpty(v)["records"])
	if len(actual) != 3 {
		t.Fatal(actual)
	}
	for i, row := range actual {
		r := fleet.ObjectOrEmpty(row)
		if fleet.Text(r["at"]) != []string{"2026-10-31T10:00:00.000Z", "2026-10-31T12:00:00.000Z", "2026-10-31T12:00:00.000Z"}[i] {
			t.Fatal(actual)
		}
		want := []Object{{"draft": fixtureID, "at": r["at"]}}
		if err := referenceWithdrawalUsage(want, usageRows, nil); err != nil {
			t.Fatal(err)
		}
		gotUsed, wantUsed := fleet.ObjectOrEmpty(r["used"]), fleet.ObjectOrEmpty(want[0]["used"])
		if intValue(gotUsed["rows"]) != intValue(wantUsed["rows"]) || intValue(gotUsed["missing"]) != intValue(wantUsed["missing"]) || gotUsed["weighted"] != wantUsed["weighted"] {
			t.Fatal(gotUsed, wantUsed)
		}
	}
	for _, q := range []string{"from=2026-02-30", "from=2026-01-01&to=2026-11-01", "project=bad"} {
		code, _ = f.call("GET", "/api/draft-withdrawals?"+q, "token", nil)
		if code != 400 {
			t.Fatal(q, code)
		}
	}
}

func BenchmarkWithdrawalUsage(b *testing.B) {
	records, rows := withdrawalFixture(20000, 100)
	for _, bench := range []struct {
		name string
		fn   func([]Object, []Object, map[string]any) error
	}{{"original", referenceWithdrawalUsage}, {"grouped", func(records, rows []Object, weights map[string]any) error {
		return applyWithdrawalUsage(records, rows, weights, nil)
	}}} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := bench.fn(copyWithdrawalRecords(records), rows, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
