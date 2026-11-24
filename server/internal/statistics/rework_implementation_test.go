package statistics

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestImplementationBreakdownNeverReusesOtherLeaseOrNewerResult(t *testing.T) {
	period := Period{accountAt("2026-11-01T00:00:00Z"), accountAt("2026-11-02T00:00:00Z")}
	now := period.To
	job := AccountRecord{"id": float64(1), "repo": "owner/repo", "issue": float64(1), "generation": float64(1), "status": "failed", "updated_at": "2026-11-01T03:00:00Z", "result": AccountRecord{"error": "固定テストが不合格"}, "view": AccountRecord{}}
	lease := AccountRecord{"job_id": float64(1), "kind": "leased", "at": "2026-11-01T00:00:00Z", "host": "nas", "data": AccountRecord{"epoch": float64(1), "generation": float64(0)}}
	terminal := AccountRecord{"job_id": float64(1), "kind": "state", "at": "2026-11-01T02:00:00Z", "host": "nas", "data": AccountRecord{"to": "failed", "epoch": float64(1)}}
	row := AccountRecord{"repo": "owner/repo", "issue": float64(1), "host": "nas", "generation": float64(1), "step": "実装", "at": "2026-11-01T01:00:00Z", "outcome": "completed"}
	report := func(events []AccountRecord) AccountRecord {
		return buildImplementationBreakdown([]AccountRecord{job}, events, []AccountRecord{row}, map[string]bool{}, period, now, 0)
	}
	got := report([]AccountRecord{lease, terminal})
	if fleet.Number(fleet.Object(got["reasons"])["unknown"]) != 1 || fleet.Number(fleet.Object(got["reasons"])["fixed-test"]) != 0 {
		t.Fatal("newer job result was reused for an older generation")
	}
	job["updated_at"] = terminal["at"]
	got = report([]AccountRecord{lease, terminal})
	if fleet.Number(fleet.Object(got["reasons"])["fixed-test"]) != 1 {
		t.Fatal("matching terminal snapshot was discarded")
	}
	// Two terminal events at the same timestamp cannot select a winning reason.
	duplicate := AccountRecord{"job_id": float64(1), "kind": "state", "at": terminal["at"], "host": "nas", "data": AccountRecord{"to": "held", "holdReason": "question", "epoch": float64(1)}}
	got = report([]AccountRecord{lease, terminal, duplicate})
	if fleet.Number(fleet.Object(got["unknownDetails"])["generationUnidentified"]) != 1 {
		t.Fatal("ambiguous terminal events were classified")
	}
	// A subsequent lease prevents an old epoch from claiming its terminal event.
	newer := AccountRecord{"job_id": float64(1), "kind": "leased", "at": "2026-11-01T01:30:00Z", "host": "nas", "data": AccountRecord{"epoch": float64(2), "generation": float64(1)}}
	got = report([]AccountRecord{lease, newer, terminal})
	if fleet.Number(fleet.Object(got["reasons"])["unknown"]) != 1 {
		t.Fatal("a terminal event crossed the lease boundary")
	}
	// Malformed engine generations remain unknown even if the lease is valid.
	row["generation"] = 1.5
	got = report([]AccountRecord{lease, terminal})
	if fleet.Number(got["unknownGenerations"]) != 1 {
		t.Fatal("fractional generation counted as an execution")
	}
}
func TestFiledTierKeepsInvalidFirstHistoryUnknown(t *testing.T) {
	rows := []AccountRecord{{"generation": float64(1), "step": "実装", "at": "2026-11-01T00:00:00Z", "tier": "light"}}
	job := AccountRecord{"tier": "heavy", "view": AccountRecord{"tierHistory": []any{AccountRecord{"from": "bad", "to": "standard"}, AccountRecord{"from": "standard", "to": "heavy"}}}}
	tier, raised := FiledTier(job, rows)
	if tier != "unknown" || !raised {
		t.Fatalf("tier=%s raised=%v", tier, raised)
	}
	_, p, e := ReworkQuery(nil, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), nil)
	if e != nil || p.To.Sub(p.From) != 14*24*time.Hour {
		t.Fatal("default period changed")
	}
}

// bothImplementationPaths runs one history through /api/stats/rework and
// /api/stats so that both classify a run under the same contract.
func bothImplementationPaths(t *testing.T, events []AccountRecord, row AccountRecord) [2]AccountRecord {
	t.Helper()
	return bothImplementationPathsWith(t, events, []AccountRecord{row}, "failed", "2026-11-01T09:00:00Z")
}

// bothImplementationPathsWith lets the job row match the terminal event, so the
// rework path reads the stale job result as its snapshot.
func bothImplementationPathsWith(t *testing.T, events, rows []AccountRecord, status, updatedAt string) [2]AccountRecord {
	t.Helper()

	db, ctx := testPostgres(t)
	period := Period{time.Date(2026, 11, 1, 0, 0, 0, 0, JST), time.Date(2026, 11, 2, 0, 0, 0, 0, JST)}
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,generation,status,updated_at,result,view) VALUES('owner/repo',1,1,$1,$2,'{"error":"固定テストが不合格"}','{}') RETURNING id`, status, accountAt(updatedAt)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		raw, err := json.Marshal(event["data"])
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,host,data) VALUES($1,$2,$3,$4,$5::jsonb)`, id, event["kind"], accountAt(event["at"]), event["host"], string(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES($1,'owner/repo',1,$2,$3,$4,$5,'test')`, fmt.Sprint(i), accountAt(row["at"]), row["step"], string(raw), row["host"])
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{DB: db, Now: func() time.Time { return period.To }}
	report, err := s.ReworkBreakdown(ctx, url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}, "project": {"owner/repo"}}, []string{"owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	rework := fleet.Object(fleet.Object(report)["implementation"])
	stats, err := s.Implementation(ctx, Query{Project: "owner/repo", Before: period, After: period})
	if err != nil {
		t.Fatal(err)
	}
	comparison := fleet.Object(fleet.Object(stats)["before"])
	for _, key := range []string{"runs", "reasons", "days", "unknownDetails", "distribution", "uc1Distribution"} {
		if !reflect.DeepEqual(normalized(rework[key]), normalized(comparison[key])) {
			t.Fatalf("%s: rework=%v comparison=%v", key, rework[key], comparison[key])
		}
	}
	return [2]AccountRecord{rework, comparison}
}

func TestImplementationPathsShareEndReasonContract(t *testing.T) {
	lease := func(at string, epoch, generation any) AccountRecord {
		return AccountRecord{"job_id": float64(1), "kind": "leased", "at": at, "host": "nas", "data": AccountRecord{"epoch": epoch, "generation": generation}}
	}
	state := func(at string, data AccountRecord) AccountRecord {
		data["epoch"] = float64(1)
		return AccountRecord{"job_id": float64(1), "kind": "state", "at": at, "host": "nas", "data": data}
	}
	end := func(reason string, fields AccountRecord) AccountRecord {
		r := AccountRecord{"version": float64(1), "generation": float64(0), "reason": reason, "usageUnknown": false}
		for k, v := range fields {
			if v == nil {
				delete(r, k)
			} else {
				r[k] = v
			}
		}
		return r
	}
	nullGeneration := end("fixed-test", nil)
	nullGeneration["generation"] = nil
	row := func(generation any) AccountRecord {
		return AccountRecord{"repo": "owner/repo", "issue": float64(1), "host": "nas", "generation": generation, "step": "実装", "at": "2026-11-01T01:00:00Z", "outcome": "completed"}
	}
	timeoutRow := row(float64(1))
	timeoutRow["outcome"] = "timeout"
	first := lease("2026-11-01T00:00:00Z", float64(1), float64(0))
	cases := []struct {
		name   string
		events []AccountRecord
		row    AccountRecord
		reason string
		detail string
	}{
		{"saved reason over conflicting hold", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "held", "holdReason": "question", "implementationEnd": end("fixed-test", nil)})}, row(float64(1)), "fixed-test", ""},
		{"saved stopped", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "stopped", "implementationEnd": end("stopped", nil)})}, row(float64(1)), "stopped", ""},
		{"stopped without record", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "stopped"})}, row(float64(1)), "stopped", ""},
		{"saved unknown is inferred", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "held", "holdReason": "review_held", "implementationEnd": end("unknown", AccountRecord{"unknownCause": "missing"})})}, row(float64(1)), "review-hold", ""},
		{"missing generation filled from lease generation 0", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("checks", AccountRecord{"generation": nil})})}, row(float64(1)), "checks", ""},
		{"null generation is not filled", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "merged", "implementationEnd": nullGeneration})}, row(float64(1)), "unknown", "unclassified"},
		{"generation mismatch", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "merged", "implementationEnd": end("fixed-test", AccountRecord{"generation": float64(1)})})}, row(float64(1)), "unknown", "unclassified"},
		{"saved expiry without matching event", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "merged", "implementationEnd": end("lease-expired", nil)})}, row(float64(1)), "unknown", "unclassified"},
		{"malformed record is not read as missing", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "merged", "implementationEnd": end("fixed-test", AccountRecord{"usageUnknown": nil})})}, row(float64(1)), "unknown", "unclassified"},
		{"lease without generation", []AccountRecord{lease("2026-11-01T00:00:00Z", float64(1), nil), state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("checks", AccountRecord{"generation": nil})})}, row(float64(1)), "unknown", "unclassified"},
		{"saved unknown ignores usage timeout", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("unknown", AccountRecord{"unknownCause": "unclassified"})})}, timeoutRow, "unknown", "unclassified"},
		{"merged without record", []AccountRecord{first, state("2026-11-01T01:30:00Z", AccountRecord{"to": "queued"}), state("2026-11-01T02:00:00Z", AccountRecord{"to": "merged"})}, row(float64(1)), "success", ""},
		{"local generation missing", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("checks", nil)})}, row(nil), "unknown", "generationUnidentified"},
		{"local generation malformed", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("checks", nil)})}, row(1.5), "unknown", "generationUnidentified"},
		{"newer lease owns the result", []AccountRecord{first, lease("2026-11-01T01:30:00Z", float64(2), float64(1)), state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("checks", nil)})}, row(float64(1)), "unknown", "reasonNotRecorded"},
		{"same time terminal events", []AccountRecord{first, state("2026-11-01T02:00:00Z", AccountRecord{"to": "failed", "implementationEnd": end("checks", nil)}), state("2026-11-01T02:00:00Z", AccountRecord{"to": "held", "holdReason": "question"})}, row(float64(1)), "unknown", "generationUnidentified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, got := range bothImplementationPaths(t, tc.events, tc.row) {
				reasons := fleet.Object(got["reasons"])
				if len(reasons) != len(jobs.ImplementationEndReasons)+1 || fleet.Number(reasons[tc.reason]) != 1 || fleet.Number(got["runs"]) != 1 {
					t.Fatalf("path %d: reasons=%v runs=%v", i, reasons, got["runs"])
				}
				if tc.detail != "" && fleet.Number(fleet.Object(got["unknownDetails"])[tc.detail]) != 1 {
					t.Fatalf("path %d: details=%v", i, got["unknownDetails"])
				}
				days, _ := normalized(got["days"]).([]any)
				day := map[string]any(nil)
				for _, d := range days {
					if fleet.Number(fleet.Object(d)["runs"]) == 1 {
						day = fleet.Object(fleet.Object(d)["reasons"])
					}
				}
				if fleet.Number(day[tc.reason]) != 1 || len(day) != len(jobs.ImplementationEndReasons)+1 {
					t.Fatalf("path %d: daily reasons=%v", i, day)
				}
			}
		})
	}
}

func TestImplementationPathsPreferMergeOverStaleResult(t *testing.T) {
	events := []AccountRecord{
		{"job_id": float64(1), "kind": "leased", "at": "2026-11-01T00:00:00Z", "host": "nas", "data": AccountRecord{"epoch": float64(1), "generation": float64(0)}},
		{"job_id": float64(1), "kind": "state", "at": "2026-11-01T02:00:00Z", "host": "nas", "data": AccountRecord{"epoch": float64(1), "to": "merged"}},
	}
	row := AccountRecord{"repo": "owner/repo", "issue": float64(1), "host": "nas", "generation": float64(1), "step": "実装", "at": "2026-11-01T01:00:00Z", "outcome": "completed"}
	for i, got := range bothImplementationPathsWith(t, events, []AccountRecord{row}, "merged", "2026-11-01T02:00:00Z") {
		reasons := fleet.Object(got["reasons"])
		if fleet.Number(reasons["success"]) != 1 || fleet.Number(reasons["fixed-test"]) != 0 {
			t.Fatalf("path %d: reasons=%v", i, reasons)
		}
	}
}

func TestImplementationPathsTreatOverlapWithUnidentifiedRunAsAmbiguous(t *testing.T) {
	end := AccountRecord{"version": float64(1), "generation": float64(0), "reason": "checks", "usageUnknown": false}
	events := []AccountRecord{
		{"job_id": float64(1), "kind": "leased", "at": "2026-11-01T00:00:00Z", "host": "nas", "data": AccountRecord{"epoch": float64(1), "generation": float64(0)}},
		{"job_id": float64(1), "kind": "state", "at": "2026-11-01T02:00:00Z", "host": "nas", "data": AccountRecord{"epoch": float64(1), "to": "failed", "implementationEnd": end}},
	}
	row := func(generation any, at string) AccountRecord {
		return AccountRecord{"repo": "owner/repo", "issue": float64(1), "host": "nas", "generation": generation, "step": "実装", "at": at, "outcome": "completed"}
	}
	// The unidentified run starts first and overlaps the generation 1 run on the same lease.
	rows := []AccountRecord{row(nil, "2026-11-01T00:10:00Z"), row(float64(1), "2026-11-01T00:30:00Z"), row(nil, "2026-11-01T00:50:00Z"), row(float64(1), "2026-11-01T01:00:00Z")}
	for i, got := range bothImplementationPathsWith(t, events, rows, "failed", "2026-11-01T09:00:00Z") {
		reasons := fleet.Object(got["reasons"])
		if fleet.Number(reasons["checks"]) != 0 || fleet.Number(reasons["unknown"]) != 2 || fleet.Number(fleet.Object(got["unknownDetails"])["generationUnidentified"]) != 2 {
			t.Fatalf("path %d: reasons=%v details=%v", i, reasons, got["unknownDetails"])
		}
	}
}

func TestImplementationPathsReviewEvidence(t *testing.T) {
	lease := func(at string, epoch float64) AccountRecord {
		return AccountRecord{"job_id": float64(1), "kind": "leased", "at": at, "host": "nas", "data": AccountRecord{"epoch": epoch, "generation": float64(0)}}
	}
	row := func(step, at, host string, generation float64) AccountRecord {
		return AccountRecord{"repo": "owner/repo", "issue": float64(1), "host": host, "generation": generation, "step": step, "at": at, "outcome": "completed"}
	}
	base := row("実装", "2026-11-01T01:00:00Z", "nas", 1)
	firstLease := lease("2026-11-01T00:00:00Z", 1)
	requeued := AccountRecord{"job_id": float64(1), "kind": "lease_expired", "at": "2026-11-01T03:00:00Z", "data": AccountRecord{"to": "queued", "leaseHost": "nas", "epoch": float64(1), "generation": float64(0)}}
	cases := []struct {
		name                      string
		events, rows              []AccountRecord
		success, unknown, expired int
	}{
		{"matching review", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 1)}, 1, 0, 0},
		{"review before requeued expiry", []AccountRecord{firstLease, requeued}, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 1)}, 1, 0, 0},
		{"expiry followed by another lease", []AccountRecord{firstLease, requeued, lease("2026-11-01T04:00:00Z", 2)}, []AccountRecord{base}, 0, 0, 1},
		{"review after requeued expiry", []AccountRecord{firstLease, requeued}, []AccountRecord{base, row("独立レビュー", "2026-11-01T04:00:00Z", "nas", 1)}, 0, 0, 1},
		{"implementation and review after requeued expiry", []AccountRecord{firstLease, requeued}, []AccountRecord{row("実装", "2026-11-01T04:00:00Z", "nas", 1), row("独立レビュー", "2026-11-01T05:00:00Z", "nas", 1)}, 0, 1, 0},
		{"other host", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "other", 1)}, 0, 1, 0},
		{"other generation", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 2)}, 0, 1, 0},
		{"other lease", []AccountRecord{lease("2026-11-01T00:00:00Z", 1), lease("2026-11-01T01:30:00Z", 2)}, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 1)}, 0, 1, 0},
		{"review before implementation", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{row("独立レビュー", "2026-11-01T00:30:00Z", "nas", 1), base}, 0, 1, 0},
		{"no review", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{base}, 0, 1, 0},
		{"no lease", nil, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 1)}, 0, 1, 0},
		{"lease host differs from implementation", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{row("実装", "2026-11-01T01:00:00Z", "other", 1), row("独立レビュー", "2026-11-01T02:00:00Z", "other", 1)}, 0, 1, 0},
		{"intervening run", []AccountRecord{lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{base, row("実装", "2026-11-01T01:30:00Z", "nas", 2), row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 1)}, 0, 2, 0},
		{"duplicate lease", []AccountRecord{lease("2026-11-01T00:00:00Z", 1), lease("2026-11-01T00:00:00Z", 1)}, []AccountRecord{base, row("独立レビュー", "2026-11-01T02:00:00Z", "nas", 1)}, 0, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, got := range bothImplementationPathsWith(t, tc.events, tc.rows, "failed", "2026-11-01T09:00:00Z") {
				reasons := fleet.Object(got["reasons"])
				if fleet.Number(reasons["success"]) != float64(tc.success) || fleet.Number(reasons["unknown"]) != float64(tc.unknown) || fleet.Number(reasons["lease-expired"]) != float64(tc.expired) {
					t.Fatalf("path %d: reasons=%v", i, reasons)
				}
			}
		})
	}
}

func TestImplementationPathsActiveLease(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,generation,lease_host,lease_epoch,lease_expires_at,view,result) VALUES('owner/repo',1,'running',1,'nas',1,$1,'{}','{}') RETURNING id`, now.Add(time.Hour)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,host,at,data) VALUES($1,'leased','nas',$2,'{"epoch":1,"generation":0}')`, id, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES('one','owner/repo',1,$1,'実装','{"repo":"owner/repo","issue":1,"step":"実装","host":"nas","generation":1,"at":"2026-11-01T02:00:00Z","outcome":"completed"}','nas','test')`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Now: func() time.Time { return now }}
	period := Period{time.Date(2026, 11, 1, 0, 0, 0, 0, JST), time.Date(2026, 11, 2, 0, 0, 0, 0, JST)}
	for _, expired := range []bool{false, true} {
		if expired {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=$1 WHERE id=$2`, now.Add(-time.Minute), id); err != nil {
				t.Fatal(err)
			}
		}
		rework, err := s.ReworkBreakdown(ctx, url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}, "project": {"owner/repo"}}, []string{"owner/repo"})
		if err != nil {
			t.Fatal(err)
		}
		comparison, err := s.Implementation(ctx, Query{Project: "owner/repo", Before: period, After: period})
		if err != nil {
			t.Fatal(err)
		}
		a, b := fleet.Object(fleet.Object(rework)["implementation"]), fleet.Object(fleet.Object(comparison)["before"])
		for _, key := range []string{"runs", "reasons", "days", "unknownDetails", "distribution", "uc1Distribution"} {
			if !reflect.DeepEqual(normalized(a[key]), normalized(b[key])) {
				t.Fatalf("%s: %v != %v", key, a[key], b[key])
			}
		}
		want := 0
		if expired {
			want = 1
		}
		if a["runs"] != want {
			t.Fatalf("expired=%v: %v", expired, a)
		}
	}
}
