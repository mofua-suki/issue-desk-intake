package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/dispatch"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeTestDispatchFencesRolesHostsAndCommits(t *testing.T) {
	macToken := "idk_" + strings.Repeat("m", 43)
	var macFleet string
	var macDocument fleet.Document
	otherToken := "idk_" + strings.Repeat("z", 43)
	handler, nativeDB, implementerToken := workerFixtureOptions(t, false, func(options *WorkerRouteOptions) {
		macFleet = options.FleetPath
		for name, token := range map[string]string{"mac-test": macToken, "mac-other": otherToken} {
			if _, err := options.AppDB.Exec("INSERT INTO api_tokens(id,name,hash,issued_at) VALUES($1,$2,$3,$4)", name, name, auth.Hash(token), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
		}
		document := `{"version":1,"defaults":{},"hosts":{"nas":{"repositories":[{"name":"owner/repo","nativeTestHost":"mac-test"}]},"mac-test":{"environment":{"platform":"darwin"},"repositories":[{"name":"owner/repo","testCommand":["sh","test.sh"]}]},"mac-other":{"environment":{"platform":"darwin"},"repositories":[{"name":"owner/repo","testCommand":["sh","test.sh"]}]}}}`
		if err := json.Unmarshal([]byte(document), &macDocument); err != nil {
			t.Fatal(err)
		}
		assignments := `{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}},"mac-test":{"role":"test-only","maxImplementations":1,"repositories":{},"testRepositories":["owner/repo"]},"mac-other":{"role":"test-only","maxImplementations":1,"repositories":{},"testRepositories":["owner/repo"]}}}`
		if err := os.WriteFile(options.FleetPath, []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(options.AssignmentsPath, []byte(assignments), 0600); err != nil {
			t.Fatal(err)
		}
	})
	head := strings.Repeat("a", 40)
	payload := map[string]any{"host": "mac-test", "head": head, "base": head, "tree": head}
	submit := map[string]any{"id": "1794530000000-aabbccdd", "kind": "native_test", "repo": "owner/repo", "payload": payload}
	raw, _ := json.Marshal(submit)
	if status, body := workerRequest(t, handler, macToken, "POST", "/api/v1/requests", string(raw)); status != http.StatusForbidden {
		t.Fatalf("test sender %d %v", status, body)
	}
	if status, body := workerRequest(t, handler, implementerToken, "POST", "/api/v1/requests", string(raw)); status != http.StatusCreated {
		t.Fatalf("submit %d %v", status, body)
	}
	if status, body := workerRequest(t, handler, otherToken, "GET", "/api/v1/requests/next?kind=native_test&repo=owner/repo", ""); status != 200 || body["request"] != nil {
		t.Fatalf("wrong target %d %v", status, body)
	}
	if status, body := workerRequest(t, handler, macToken, "GET", "/api/v1/requests/next?kind=pr_create&repo=owner/repo", ""); status != 403 {
		t.Fatalf("implementation claim %d %v", status, body)
	}
	if status, body := workerRequest(t, handler, macToken, "POST", "/api/v1/jobs/next", `{"host":"mac-test","capacity":1}`); status != 403 {
		t.Fatalf("job claim %d %v", status, body)
	}
	status, body := workerRequest(t, handler, macToken, "GET", "/api/v1/requests/next?kind=native_test&repo=owner/repo", "")
	if status != 200 || fleet.Object(body["request"])["id"] != submit["id"] {
		t.Fatalf("claim %d %v", status, body)
	}
	result := map[string]any{"status": "passed", "head": head, "base": head, "tree": strings.Repeat("b", 40), "message": "ok"}
	send := func(token string) int {
		raw, _ := json.Marshal(map[string]any{"result": result})
		status, _ := workerRequest(t, handler, token, "POST", "/api/v1/requests/1794530000000-aabbccdd/result", string(raw))
		return status
	}
	if status := send(macToken); status != 409 {
		t.Fatalf("wrong tree %d", status)
	}
	result["tree"] = head
	if status := send(otherToken); status != 403 {
		t.Fatalf("wrong actor %d", status)
	}
	if status := send(macToken); status != 200 {
		t.Fatalf("result %d", status)
	}
	if status := send(macToken); status != 200 {
		t.Fatalf("idempotent result %d", status)
	}
	result["status"] = "failed"
	if status := send(macToken); status != 409 {
		t.Fatalf("changed result %d", status)
	}
	// Both manual PRs and existing Issue leases must declare the current gate.
	manualID := "1794530000008-aabbccdd"
	rawManual, _ := json.Marshal(map[string]any{"id": manualID, "kind": "merge_manual", "repo": "owner/repo", "payload": map[string]any{"number": float64(1), "head": head}})
	if status, body := workerRequest(t, handler, implementerToken, "POST", "/api/v1/requests", string(rawManual)); status != 201 {
		t.Fatalf("manual submit %d %v", status, body)
	}
	if status, body := workerRequest(t, handler, implementerToken, "GET", "/api/v1/requests/next?kind=merge_manual&repo=owner/repo", ""); status != 200 {
		t.Fatalf("manual claim %d %v", status, body)
	}
	_, cfg := workerRequest(t, handler, implementerToken, "GET", "/api/v1/client-config", "")
	gate := map[string]any{"version": float64(1), "host": "mac-test", "configurationVersion": fleet.Object(fleet.Object(cfg["config"])["nativeTestVersions"])["mac-test"]}
	var jobID int64
	if err := nativeDB.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at) VALUES('owner/repo',1,'running','nas',1,now()+interval '10 minutes') RETURNING id`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	for _, who := range []map[string]any{{"requestId": manualID}, {"jobId": jobID, "epoch": int64(1)}} {
		turnRepo := "owner/repo"
		call := func(action string) (int, map[string]any) {
			raw, _ := json.Marshal(who)
			return workerRequest(t, handler, implementerToken, "POST", "/api/v1/merge-turns/"+turnRepo+"/"+action, string(raw))
		}
		for _, casing := range []string{"owner/repo", "OWNER/REPO"} {
			turnRepo = casing
			if status, body := call("acquire"); status != 409 || body["code"] != "NATIVE_TEST_GATE_REQUIRED" {
				t.Fatalf("old acquire %s %d %v", casing, status, body)
			}
		}
		who["nativeTestGate"] = gate
		status, body := call("acquire")
		if status != 200 || body["granted"] != true {
			t.Fatalf("current acquire %d %v", status, body)
		}
		who["turnEpoch"] = fleet.Object(body["turn"])["epoch"]
		delete(who, "nativeTestGate")
		for _, casing := range []string{"owner/repo", "OWNER/REPO"} {
			turnRepo = casing
			if status, body := call("renew"); status != 409 {
				t.Fatalf("old renew %s %d %v", casing, status, body)
			}
		}
		who["nativeTestGate"] = map[string]any{"version": float64(1), "host": "mac-test", "configurationVersion": strings.Repeat("0", 16)}
		if status, body := call("renew"); status != 409 {
			t.Fatalf("stale policy %d %v", status, body)
		}
		who["nativeTestGate"] = gate
		if status, body := call("renew"); status != 200 {
			t.Fatalf("current renew %d %v", status, body)
		}
		delete(who, "nativeTestGate")
		if status, body := call("release"); status != 200 {
			t.Fatalf("release %d %v", status, body)
		}
	}
	status, before := workerRequest(t, handler, implementerToken, "GET", "/api/v1/client-config", "")
	if status != 200 {
		t.Fatalf("before config %d %v", status, before)
	}
	fleet.Object(fleet.List(macDocument.Hosts["mac-test"]["repositories"])[0])["testCommand"] = []any{"sh", "changed-test.sh"}
	rawFleet, _ := json.Marshal(macDocument)
	if err := os.WriteFile(macFleet, rawFleet, 0600); err != nil {
		t.Fatal(err)
	}
	status, after := workerRequest(t, handler, implementerToken, "GET", "/api/v1/client-config", "")
	if status != 200 || before["configurationVersion"] == after["configurationVersion"] {
		t.Fatalf("Macコマンド変更が実装担当へ伝わりません: %d %v", status, after)
	}

}

func TestNativeQueueSkipsOldAssignmentWithoutBlockingReplacement(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprint(claimed), func(t *testing.T) {
			_, db, _ := workerFixture(t)
			queue := dispatch.Queue{DB: db}
			ctx := context.Background()
			head := strings.Repeat("a", 40)
			payload := map[string]any{"host": "mac-test", "head": head, "base": head, "tree": head}
			if _, _, err := queue.Submit(ctx, dispatch.Request{ID: "1794530000001-aabbccdd", Kind: "native_test", Repo: "owner/repo", Payload: payload, Sender: "old"}); err != nil {
				t.Fatal(err)
			}
			if claimed {
				if _, err := queue.NextNative(ctx, "mac-test", []string{"owner/repo"}, map[string][]string{"owner/repo": {"old"}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := queue.Submit(ctx, dispatch.Request{ID: "1794530000002-aabbccdd", Kind: "native_test", Repo: "owner/repo", Payload: payload, Sender: "replacement"}); err != nil {
				t.Fatal(err)
			}
			next, err := queue.NextNative(ctx, "mac-test", []string{"owner/repo"}, map[string][]string{"owner/repo": {"replacement"}})
			if err != nil || next == nil || next.Sender != "replacement" {
				t.Fatalf("next=%+v err=%v", next, err)
			}
			old, err := queue.GetRequest(ctx, "1794530000001-aabbccdd")
			expected := "queued"
			if claimed {
				expected = "claimed"
			}
			if err != nil || old.Status != expected {
				t.Fatalf("old=%+v err=%v", old, err)
			}
			if claimed {
				if _, err := queue.RequestAction(ctx, old.ID, "mac-test", "release", nil); err != nil {
					t.Fatal(err)
				}
			}
			if cancelled, err := queue.RequestAction(ctx, old.ID, "old", "cancel", nil); err != nil || cancelled == nil {
				t.Fatalf("cancel=%+v err=%v", cancelled, err)
			}
		})
	}
}

func TestConcurrentNativeResultsRejectDifferentFinalState(t *testing.T) {
	_, db, _ := workerFixture(t)
	queue := dispatch.Queue{DB: db}
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	id := "1794530000003-aabbccdd"
	payload := map[string]any{"host": "mac-test", "head": head, "base": head, "tree": head}
	if _, _, err := queue.Submit(ctx, dispatch.Request{ID: id, Kind: "native_test", Repo: "owner/repo", Payload: payload, Sender: "nas"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.NextNative(ctx, "mac-test", []string{"owner/repo"}, map[string][]string{"owner/repo": {"nas"}}); err != nil {
		t.Fatal(err)
	}
	lock, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var holder int
	if err := lock.QueryRow("SELECT pg_backend_pid() FROM native_test_requests WHERE id=$1 FOR UPDATE", id).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, state := range []string{"passed", "failed"} {
		go func() {
			_, err := queue.RequestAction(ctx, id, "mac-test", "result", map[string]any{"status": state, "head": head, "base": head, "tree": head, "message": "done"})
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	blocked := 0
	for time.Now().Before(deadline) {
		if err := db.QueryRow("WITH RECURSIVE blocked(pid) AS (SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) UNION SELECT a.pid FROM pg_stat_activity a JOIN blocked b ON b.pid=ANY(pg_blocking_pids(a.pid))) SELECT count(*) FROM blocked", holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if blocked < 2 {
		t.Fatal("同時送信のUPDATE待ちを再現できませんでした")
	}
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if p, ok := err.(*contract.Problem); ok && p.Status == 409 {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
}

func TestNativeQueueOldSnapshotCannotFinalizeNewSenderRequest(t *testing.T) {
	_, db, _ := workerFixture(t)
	queue := dispatch.Queue{DB: db}
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	id := "1794530000004-aabbccdd"
	_, _, err := queue.Submit(ctx, dispatch.Request{ID: id, Kind: "native_test", Repo: "owner/repo", Sender: "replacement", Payload: map[string]any{"host": "mac-test", "head": head, "base": head, "tree": head}})
	if err != nil {
		t.Fatal(err)
	}
	next, err := queue.NextNative(ctx, "mac-test", []string{"owner/repo"}, map[string][]string{"owner/repo": {"old"}})
	if err != nil || next != nil {
		t.Fatalf("old snapshot next=%+v err=%v", next, err)
	}
	next, err = queue.NextNative(ctx, "mac-test", []string{"owner/repo"}, map[string][]string{"owner/repo": {"replacement"}})
	if err != nil || next == nil || next.ID != id {
		t.Fatalf("new snapshot next=%+v err=%v", next, err)
	}
}
