package jobs

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"testing"
	"time"
)

func TestTransferReportRejectsMalformedExplicitTransfers(t *testing.T) {
	job := &Job{Repo: "owner/repo", Issue: 1}
	for _, raw := range []string{`{"acceptanceTransfers":null}`, `{"acceptanceTransfers":[{"criterion":"c","artifacts":["a"],"followupNumbers":[1]}],"followupIssues":[]}`, `{"acceptanceTransfers":[],"followupIssues":[{"title":"t","purpose":"p","acceptance":[]}]}`} {
		var result map[string]any
		_ = json.Unmarshal([]byte(raw), &result)
		if _, err := transferBindings(job, result); err == nil {
			t.Fatalf("accepted malformed transfer %s", raw)
		}
	}
	if saved, err := transferBindings(job, map[string]any{}); err != nil || saved != nil {
		t.Fatal("old result rejected", saved, err)
	}
}
func TestTransferRejectionHistoryIsReadWithoutRestoringOrFiling(t *testing.T) {
	app := pgtest.New(t)
	job := &Job{Repo: "owner/repo", Issue: 1, View: map[string]any{}}
	var result map[string]any
	_ = json.Unmarshal([]byte(`{"acceptanceTransfers":[{"criterion":"finish","artifacts":["artifact"],"followupNumbers":[1]}],"followupIssues":[{"title":"#1 次の依頼","purpose":"preserve & <scope>","acceptance":["verified"]}]}`), &result)
	binding, err := transferBindings(job, result)
	if err != nil {
		t.Fatal(err)
	}
	canonical := fleet.Object(fleet.Clone(binding))
	b := fleet.Object(fleet.List(canonical["proposals"])[0])
	expected := fleet.Object(fleet.List(fleet.Object(canonical["report"])["followupIssues"])[0])
	fingerprints := transferFingerprints(canonical, b, expected)
	record := map[string]any{"id": b["id"], "type": "rejection", "repo": "owner/repo", "number": 1, "fingerprint": fingerprints[0]}
	if _, err = app.Exec("INSERT INTO proposals(id,json)VALUES('different-storage-key',?)", transferJSON(record)); err != nil {
		t.Fatal(err)
	}
	s := &Store{options: Options{AppDB: app}}
	resolved, err := s.resolveTransferRecords(context.Background(), canonical)
	if err != nil {
		t.Fatal(err)
	}
	proposal := fleet.Object(fleet.List(resolved["proposals"])[0])
	if proposal["status"] != "rejected" {
		t.Fatalf("history not matched %v", proposal)
	}
	var count int
	if err = app.QueryRow("SELECT count(*)FROM drafts").Scan(&count); err != nil || count != 0 {
		t.Fatal("reconciliation modified drafts", count, err)
	}
	expected["purpose"] = "changed"
	canonical["report"] = map[string]any{"acceptanceTransfers": fleet.Object(canonical["report"])["acceptanceTransfers"], "followupIssues": []any{expected}}
	resolved, err = s.resolveTransferRecords(context.Background(), canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Object(fleet.List(resolved["proposals"])[0])["status"] == "rejected" {
		t.Fatal("different content borrowed rejection evidence")
	}
}

func TestReconcileTransferRecordsSkipsSettledCompletedJobs(t *testing.T) {
	db, ctx := testPostgres(t)
	app := pgtest.New(t)
	s := &Store{db: db, options: Options{AppDB: app}}
	before, err := s.transferReconciliationCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	records, err := transferBindings(&Job{Repo: "owner/repo", Issue: 1}, map[string]any{"acceptanceTransfers": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	view := transferJSON(map[string]any{"acceptanceTransferRecords": records})
	for i := 0; i < 10; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,view) VALUES('owner/repo',$1,'merged',$2::jsonb)`, i+1, view); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.transferReconciliationCandidates(ctx)
	if err != nil || len(after) != len(before) {
		t.Fatalf("candidates before=%v after=%v err=%v", before, after, err)
	}
	if err := s.ReconcileTransferRecords(ctx); err != nil {
		t.Fatal(err)
	}
	var changed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE view <> $1::jsonb`, view).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
}

func TestReconcileTransferRecordsKeepsUnsettledJobs(t *testing.T) {
	db, ctx := testPostgres(t)
	app := pgtest.New(t)
	s := &Store{db: db, options: Options{AppDB: app}}
	job := &Job{Repo: "owner/repo", Issue: 1}
	var result map[string]any
	if err := json.Unmarshal([]byte(`{"acceptanceTransfers":[{"criterion":"c","artifacts":["a"],"followupNumbers":[1]}],"followupIssues":[{"title":"t","purpose":"p","acceptance":["a"]}]}`), &result); err != nil {
		t.Fatal(err)
	}
	records, err := transferBindings(job, result)
	if err != nil {
		t.Fatal(err)
	}
	fleet.Object(fleet.List(records["proposals"])[0])["reason"] = "old"
	view := transferJSON(map[string]any{"acceptanceTransferRecords": records})
	empty := `{"acceptanceTransferRecords":{"source":"owner/repo#2","report":{"followupIssues":[]},"proposals":[]}}`
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,view) VALUES('owner/repo',1,'merged',$1::jsonb),('owner/repo',2,'running',$2::jsonb)`, view, empty); err != nil {
		t.Fatal(err)
	}
	ids, err := s.transferReconciliationCandidates(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatalf("candidates=%v err=%v", ids, err)
	}
	if err := s.ReconcileTransferRecords(ctx); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := db.QueryRowContext(ctx, `SELECT view->'acceptanceTransferRecords'->'proposals'->0->>'reason' FROM jobs WHERE issue=1`).Scan(&reason); err != nil || reason != "対応する受付の案記録がありません" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}

	// A rejection is not terminal: a later observation changes its evidence.
	b := fleet.Object(fleet.List(records["proposals"])[0])
	expected := fleet.Object(fleet.List(fleet.Object(records["report"])["followupIssues"])[0])
	fingerprint := transferFingerprints(records, b, expected)[0]
	rejection := map[string]any{"id": b["id"], "type": "rejection", "repo": "owner/repo", "number": 1, "fingerprint": fingerprint}
	if _, err := app.Exec("INSERT INTO proposals(id,json)VALUES(?,?)", "history1", transferJSON(rejection)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTransferRecords(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT view->'acceptanceTransferRecords'->'proposals'->0->>'status' FROM jobs WHERE issue=1`).Scan(&status); err != nil || status != "rejected" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if _, err := app.Exec("INSERT INTO proposals(id,json)VALUES(?,?)", "history2", transferJSON(rejection)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileTransferRecords(ctx); err != nil {
		t.Fatal(err)
	}
	var evidence int
	if err := db.QueryRowContext(ctx, `SELECT jsonb_array_length(view->'acceptanceTransferRecords'->'proposals'->0->'evidence') FROM jobs WHERE issue=1`).Scan(&evidence); err != nil || evidence != 2 {
		t.Fatalf("evidence=%d err=%v", evidence, err)
	}
}

func TestSplitTitleKeepsBehavior(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"#1 次の依頼", "次の依頼"}, {"題名（ 中身 ） [ 内容 ]", "題名（中身） [内容]"}, {"#123 （ ）", "依頼の続き"},
	} {
		if got := splitTitle(tc.input); got != tc.want {
			t.Errorf("splitTitle(%q)=%q want %q", tc.input, got, tc.want)
		}
	}
}

// TestTransferCompletionReportRecovery exercises persisted legacy reports and terminal resends.
func TestTransferCompletionReportRecovery(t *testing.T) {
	for _, legacyFirst := range []bool{true, false} {
		name := "normalized-first"
		if legacyFirst {
			name = "legacy-first"
		}
		t.Run(name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			makeResult := func(criterion, artifact string) map[string]any {
				return map[string]any{
					"mergeCommit":         "abc",
					"acceptanceTransfers": []any{map[string]any{"criterion": criterion, "artifacts": []any{artifact}, "followupNumbers": []any{float64(1)}}},
					"followupIssues":      []any{map[string]any{"title": "next", "purpose": "finish", "acceptance": []any{"done"}}, map[string]any{"title": "other", "purpose": "other work", "acceptance": []any{"done"}}},
				}
			}
			clean := makeResult("criterion", "artifact")
			legacy := makeResult(" criterion ", " artifact ")
			first, second := clean, legacy
			if legacyFirst {
				first, second = legacy, clean
			}
			records, err := transferBindings(&Job{Repo: "owner/repo", Issue: 1}, first)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate a stored report using untrimmed request values.
			records["report"] = map[string]any{"acceptanceTransfers": first["acceptanceTransfers"], "followupIssues": first["followupIssues"]}
			_, err = db.ExecContext(ctx, `UPDATE jobs SET view=view || $2::jsonb WHERE id=$1`, id, transferJSON(map[string]any{"acceptanceTransferRecords": records, "finalApprovals": []any{map[string]any{"id": "approval"}}}))
			if err != nil {
				t.Fatal(err)
			}
			var before string
			if err = db.QueryRowContext(ctx, `SELECT jsonb_build_object('records',view->'acceptanceTransferRecords','approvals',view->'finalApprovals','host',lease_host,'epoch',lease_epoch,'expires',lease_expires_at)::text FROM jobs WHERE id=$1`, id).Scan(&before); err != nil {
				t.Fatal(err)
			}
			bad := makeResult("changed", "artifact")
			_, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "merged", Result: bad})
			code(t, err, 409, "acceptance_transfer_conflict")
			wrongTarget := makeResult("criterion", "artifact")
			fleet.Object(fleet.List(wrongTarget["acceptanceTransfers"])[0])["followupNumbers"] = []any{float64(2)}
			_, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "merged", Result: wrongTarget})
			code(t, err, 409, "acceptance_transfer_conflict")
			_, err = s.Report(ctx, id, "other", Report{Epoch: 7, Status: "merged", Result: second})
			code(t, err, 409, "lease_lost")
			_, err = s.Report(ctx, id, "nas", Report{Epoch: 6, Status: "merged", Result: second})
			code(t, err, 409, "lease_lost")
			invalid := makeResult("criterion", "artifact")
			invalid["acceptanceTransfers"] = nil
			_, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "merged", Result: invalid})
			code(t, err, 400, "REQUEST_REJECTED")
			var after string
			if err = db.QueryRowContext(ctx, `SELECT jsonb_build_object('records',view->'acceptanceTransferRecords','approvals',view->'finalApprovals','host',lease_host,'epoch',lease_epoch,'expires',lease_expires_at)::text FROM jobs WHERE id=$1`, id).Scan(&after); err != nil || before != after {
				t.Fatalf("rejection changed saved record, approval or lease: %v", err)
			}
			report := Report{Epoch: 7, Status: "merged", Result: second}
			job, err := s.Report(ctx, id, "nas", report)
			if err != nil || job.Status != "merged" {
				t.Fatalf("completion: job=%+v err=%v", job, err)
			}
			report.Result = first
			if _, err = s.Report(ctx, id, "nas", report); err != nil {
				t.Fatalf("equivalent replay: %v", err)
			}
			report.Result = bad
			_, err = s.Report(ctx, id, "nas", report)
			code(t, err, 409, "acceptance_transfer_conflict")
			report.Result = makeResult("criterion", "artifact")
			report.Result["mergeCommit"] = "different"
			_, err = s.Report(ctx, id, "nas", report)
			code(t, err, 409, "lease_lost")
			var count int
			var stored bool
			if err = db.QueryRowContext(ctx, `SELECT (view->'acceptanceTransferRecords')=$2::jsonb AND (view->'finalApprovals')=$3::jsonb FROM jobs WHERE id=$1 AND status='merged'`, id, transferJSON(records), `[{"id":"approval"}]`).Scan(&stored); err != nil || !stored {
				t.Fatalf("saved transfer or approval changed: %v", err)
			}
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='merged'`, id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("completion events=%d err=%v", count, err)
			}
		})
	}
	t.Run("completion-creates-transfer", func(t *testing.T) {
		db, ctx := testPostgres(t)
		id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
		s, err := New(db, Options{Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]any{
			"mergeCommit":         "abc",
			"acceptanceTransfers": []any{map[string]any{"criterion": "  criterion ", "artifacts": []any{" artifact "}, "followupNumbers": []any{float64(1)}}},
			"followupIssues":      []any{map[string]any{"title": "next", "purpose": "finish", "acceptance": []any{"done"}}},
		}
		report := Report{Epoch: 7, Status: "merged", Result: result}
		if _, err = s.Report(ctx, id, "nas", report); err != nil {
			t.Fatal(err)
		}
		result["acceptanceTransfers"] = []any{map[string]any{"criterion": "criterion", "artifacts": []any{"artifact"}, "followupNumbers": []any{float64(1)}}}
		if _, err = s.Report(ctx, id, "nas", report); err != nil {
			t.Fatalf("normalized replay: %v", err)
		}
		var count int
		var criterion string
		if err = db.QueryRowContext(ctx, `SELECT view->'acceptanceTransferRecords'->'report'->'acceptanceTransfers'->0->>'criterion' FROM jobs WHERE id=$1 AND status='merged'`, id).Scan(&criterion); err != nil || criterion != "criterion" {
			t.Fatalf("transfer not persisted: %q %v", criterion, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='merged'`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("completion events=%d err=%v", count, err)
		}
	})
}
