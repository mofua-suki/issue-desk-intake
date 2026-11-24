package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFullTestsIgnoreLegacyRecords(t *testing.T) {
	const legacy = "mofua-suki/" + "issue-desk"
	failed := map[string]any{"sha": strings.Repeat("a", 40), "startedAt": "2026-10-31T00:00:00Z", "finishedAt": "2026-10-31T00:01:00Z", "status": "failed", "ms": float64(60000), "failedFiles": []any{"a.test.mjs"}}
	passed := map[string]any{"sha": strings.Repeat("b", 40), "startedAt": "2026-11-01T00:00:00Z", "status": "passed"}
	write := func(t *testing.T, s *Service, raw []byte) {
		t.Helper()
		dir := filepath.Join(s.RuntimeDir, "state")
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dir, "full-test-run.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	table := func(t *testing.T, s *Service, result map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"mainBreakResult": result})
		if _, e := s.DB.Exec("INSERT INTO full_test_intake(repo,json) VALUES(?,?) ON CONFLICT(repo) DO UPDATE SET json=excluded.json", legacy, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	record, _ := json.Marshal(map[string]any{"v": 1, "repo": legacy, "last": failed})
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *Service)
	}{
		{"file", func(t *testing.T, s *Service) { write(t, s, record) }},
		{"table", func(t *testing.T, s *Service) { table(t, s, failed) }},
		{"broken_json", func(t *testing.T, s *Service) { write(t, s, []byte("{")) }},
		{"newer_success_and_old_failure", func(t *testing.T, s *Service) { write(t, s, record); table(t, s, passed) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := fixture(t)
			test.prepare(t, s)
			rows, e := s.FullTests(context.Background())
			if e != nil || rows == nil || len(rows) != 0 {
				t.Fatal(rows, e)
			}
		})
	}
}
func TestRuntimeAlertBoundaries(t *testing.T) {
	s := fixture(t)
	dir := filepath.Join(s.RuntimeDir, "state")
	os.MkdirAll(dir, 0700)
	record := map[string]any{"total": float64(2), "failures": float64(1), "longestFailureMs": float64(30000), "revision": strings.Repeat("a", 40), "completedAt": s.now().Add(-7 * 24 * time.Hour).UnixMilli()}
	raw, _ := json.Marshal(record)
	os.WriteFile(filepath.Join(dir, "intake-handoff.json"), raw, 0600)
	if rows := s.runtimeAlerts(); len(rows) != 1 {
		t.Fatal(rows)
	}
	record["completedAt"] = s.now().Add(-7*24*time.Hour - time.Millisecond).UnixMilli()
	raw, _ = json.Marshal(record)
	os.WriteFile(filepath.Join(dir, "intake-handoff.json"), raw, 0600)
	if rows := s.runtimeAlerts(); len(rows) != 0 {
		t.Fatal("expired runtime alert", rows)
	}
}
func TestPostgresUsageLimitStaleDisappearsWithMeasuredZero(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	ctx := context.Background()
	at := s.now().Add(-6 * time.Hour)
	if _, e := s.PostgreSQL.Exec("INSERT INTO usage_rows(key,at,host,source,provider,json,sender)VALUES('call',$1,'worker','worker','codex',$2,'worker')", at, `{"at":"`+iso(at)+`","provider":"codex"}`); e != nil {
		t.Fatal(e)
	}
	rows, e := s.usageStaleAlerts(ctx)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	if _, e = s.PostgreSQL.Exec("INSERT INTO usage_limit_rows(key,at,host,source,provider,\"limit\",json,sender)VALUES('limit',$1,'worker','worker','codex','seven_day',$2,'worker')", at, `{"at":"`+iso(at)+`","provider":"codex","limit":"seven_day","percent":0}`); e != nil {
		t.Fatal(e)
	}
	rows, e = s.usageStaleAlerts(ctx)
	if e != nil || len(rows) != 0 {
		t.Fatal("measured zero treated missing", rows, e)
	}
}

func TestHeavyReworkCountsOnlyKnownIssuesAndStrictThreshold(t *testing.T) {
	tier := map[string]any{"key": "heavy", "issues": float64(5), "unknownIssues": float64(2), "reworkRate": 0.5}
	if rows := HeavyRework(tier, nil, nil); len(rows) != 0 {
		t.Fatal("threshold equality alerted")
	}
	tier["reworkRate"] = 2.0 / 3
	if rows := HeavyRework(tier, nil, nil); len(rows) != 1 || fleet.Number(fleet.Object(rows[0])["count"]) != 3 {
		t.Fatal(rows)
	}
	tier["unknownIssues"] = float64(3)
	if rows := HeavyRework(tier, nil, nil); len(rows) != 0 {
		t.Fatal("insufficient known sample alerted")
	}
}
func TestContractMismatchDoesNotCountUpdatingAndExpires(t *testing.T) {
	s := fixture(t)
	var kept ContractMismatches
	hash := strings.Repeat("b", 64)
	for range 4 {
		kept.Record(hash, true, s.now())
	}
	if rows := kept.Alerts(s.RuntimeDir, s.now()); len(rows) != 0 {
		t.Fatal(rows)
	}
	kept.Record(hash, false, s.now())
	kept.Record(hash, false, s.now())
	if rows := kept.Alerts(s.RuntimeDir, s.now().Add(30*time.Minute)); len(rows) != 1 {
		t.Fatal(rows)
	}
	if rows := kept.Alerts(s.RuntimeDir, s.now().Add(30*time.Minute+time.Millisecond)); len(rows) != 0 {
		t.Fatal(rows)
	}
	dir := filepath.Join(s.RuntimeDir, "state")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "desk-contract-update.until"), []byte(fmt.Sprint(s.now().Add(15*time.Minute).UnixMilli())), 0600)
	if !ContractUpdating(s.RuntimeDir, s.now()) || len(kept.Alerts(s.RuntimeDir, s.now())) != 0 {
		t.Fatal("update marker ignored")
	}
}
