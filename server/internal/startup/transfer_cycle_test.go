package startup

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/jobs"
)

// transferCycleJobs は、移管元（マージ済み）・移管先（起票済み）と、移管元に依存して待つ依頼を保存する。
func transferCycleJobs(t *testing.T) *sql.DB {
	t.Helper()
	db := appPostgres(t)
	recordsJSON := `{"source":"owner/repo#1","report":{"acceptanceTransfers":[{"criterion":"required artifact","artifacts":["output"],"followupNumbers":[1]}]},"proposals":[{"id":"p","number":1,"status":"filed","issue":{"project":"owner/repo","number":2}}]}`
	if _, err := db.ExecContext(t.Context(), `INSERT INTO jobs(id,repo,issue,title,status,view)VALUES(1,'owner/repo',1,'source','merged',jsonb_build_object('acceptanceTransferRecords',$1::jsonb)),(2,'owner/repo',2,'producer','closed','{}'),(3,'owner/repo',3,'consumer','queued','{}')`, recordsJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO job_deps(job_id,depends_on)VALUES(3,1)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func transferCycleWait(t *testing.T, store *jobs.Store) map[string]any {
	t.Helper()
	job, err := store.Get(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	return fleet.Object(job.View["acceptanceTransferWait"])
}

func TestPostgresAcceptanceTransfersRefreshOnceWithoutChecks(t *testing.T) {
	db := transferCycleJobs(t)
	var reads atomic.Int32
	store, err := jobs.New(db, jobs.Options{ReadIssue: func(context.Context, string, int64) (map[string]any, error) {
		reads.Add(1)
		return nil, errors.New("Giteaへ接続できません")
	}})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Config: &Config{}, Jobs: store}
	for cycle := int32(1); cycle <= 2; cycle++ {
		if err = app.resolveAcceptanceTransfers(t.Context()); err != nil {
			t.Fatal(err)
		}
		// 本文を読めない依頼は相談へ回さないので、同じ周期の中では読み直さない。
		if got := reads.Load(); got != cycle {
			t.Fatalf("%d 周目までのIssueの読取が %d 回", cycle, got)
		}
		if wait := transferCycleWait(t, store); wait["blocking"] != true {
			t.Fatalf("本文を読めない依頼の開始を止めていない: %v", wait)
		}
	}
	checks, err := store.TransferChecks(t.Context())
	if err != nil || len(checks) != 0 {
		t.Fatalf("本文を読めない依頼を相談へ回した: %v %v", checks, err)
	}
}

func TestPostgresAcceptanceTransfersRefreshAfterReconcileFailure(t *testing.T) {
	db := transferCycleJobs(t)
	local := newAppStore(t)
	var reads atomic.Int32
	// 受付の案を照合できる保存先があると、followupIssues の無い保存済みの記録は照合に失敗する。
	store, err := jobs.New(db, jobs.Options{AppDB: local.DB(), ReadIssue: func(context.Context, string, int64) (map[string]any, error) {
		reads.Add(1)
		return map[string]any{"number": float64(3), "body": "依頼はoutputを利用する"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reconcileErr := store.ReconcileTransferRecords(t.Context())
	if reconcileErr == nil {
		t.Fatal("移管記録の照合が失敗する前提を作れていない")
	}
	app := &App{Config: &Config{}, Jobs: store}
	if err = app.resolveAcceptanceTransfers(t.Context()); err == nil || err.Error() != reconcileErr.Error() {
		t.Fatalf("照合の失敗を返していない: %v", err)
	}
	// 周期の保守が重ねて行わなくなったので、照合に失敗した周期でも開始条件はここで更新する。
	if wait := transferCycleWait(t, store); wait["blocking"] != true || len(fleet.List(wait["checks"])) != 1 {
		t.Fatalf("照合に失敗した周期で開始条件を更新していない: %v", wait)
	}
	// 相談の照合は次の周期へ回す（Records・Consultations を持たないこの App で進めば落ちる）。
	if got := reads.Load(); got != 1 {
		t.Fatalf("Issueの読取が %d 回", got)
	}
}

func TestPostgresWorkerMaintenanceLeavesTransfersToTheirCycle(t *testing.T) {
	db := transferCycleJobs(t)
	var reads atomic.Int32
	readIssue := func(context.Context, string, int64) (map[string]any, error) {
		reads.Add(1)
		return nil, errors.New("Giteaへ接続できません")
	}
	root := filepath.Join("..", "..", "..")
	if err := httpapi.WorkerMaintenance(t.Context(), httpapi.WorkerRouteOptions{DB: db, FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json"), ReadIssue: readIssue}); err != nil {
		t.Fatal(err)
	}
	store, err := jobs.New(db, jobs.Options{ReadIssue: readIssue})
	if err != nil {
		t.Fatal(err)
	}
	// 30秒ごとの保守は移管の開始条件を読み直さない。同じ間隔の acceptance-transfers が受け持つ。
	if wait := transferCycleWait(t, store); reads.Load() != 0 || wait != nil {
		t.Fatalf("保守が移管の開始条件を更新した: 読取 %d 回、%v", reads.Load(), wait)
	}
	if err = (&App{Config: &Config{}, Jobs: store}).resolveAcceptanceTransfers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if wait := transferCycleWait(t, store); reads.Load() != 1 || wait["blocking"] != true {
		t.Fatalf("acceptance-transfers が開始条件を更新していない: 読取 %d 回、%v", reads.Load(), wait)
	}
}
