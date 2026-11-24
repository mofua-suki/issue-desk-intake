package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkerRunningPublicationReceiptAndDispatch(t *testing.T) {
	handler, db, token := workerFixture(t)
	head := strings.Repeat("a", 40)
	if _, err := db.Exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,2)`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status,generation,scope,lease_host,lease_epoch,lease_expires_at,branch,pr,head_sha,view) VALUES
(1,'owner/repo',1,'running',1,ARRAY['src/'],'nas',1,$1,'work',12,$2,'{"phase":"running","reimplementExpected":{"generation":1,"reimplements":true}}'),
(2,'owner/repo',2,'queued',0,ARRAY['src/free.go'],NULL,0,NULL,NULL,NULL,NULL,'{}')`, time.Now().Add(time.Hour), head); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,data) VALUES(1,'control','{"action":"retry","generation":1,"instruction":true}')`); err != nil {
		t.Fatal(err)
	}
	status, blocked := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"capacity":2}`)
	if status != 204 || blocked["job"] != nil {
		t.Fatalf("reimplementation dispatch: %d %v", status, blocked)
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/v1/jobs/1/events", `{"epoch":1,"kind":"running_publication","data":{"generation":1,"epoch":1,"pr":12,"branch":"work","headSha":"`+head+`"}}`)
	if status != 400 {
		t.Fatalf("forged publication status=%d", status)
	}
	payload, _ := json.Marshal(map[string]any{"epoch": 1, "status": "running", "pr": 12, "branch": "work", "headSha": head, "view": map[string]any{"phase": "publishing"}})
	status, reported := workerRequest(t, handler, token, "POST", "/api/v1/jobs/1/state", string(payload))
	if status != 200 {
		t.Fatalf("publication report: %d %v", status, reported)
	}
	// Repeated state reports do not manufacture duplicate publication receipts.
	status, _ = workerRequest(t, handler, token, "POST", "/api/v1/jobs/1/state", string(payload))
	if status != 200 {
		t.Fatalf("repeated report: %d", status)
	}
	status, detail := workerRequest(t, handler, token, "GET", "/api/v1/jobs/1", "")
	if status != 200 {
		t.Fatalf("detail: %d %v", status, detail)
	}
	receipts := 0
	for _, raw := range detail["events"].([]any) {
		event := raw.(map[string]any)
		if event["kind"] != "running_publication" {
			continue
		}
		receipts++
		data := event["data"].(map[string]any)
		if event["jobId"] != float64(1) || event["host"] != "nas" || data["generation"] != float64(1) || data["epoch"] != float64(1) || data["pr"] != float64(12) || data["branch"] != "work" || data["headSha"] != head {
			t.Fatalf("publication event=%v", event)
		}
	}
	if receipts != 1 {
		t.Fatalf("publication receipts=%d", receipts)
	}
	status, taken := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"capacity":2}`)
	if status != 200 || taken["job"] == nil || taken["job"].(map[string]any)["id"] != float64(2) || taken["lease"] == nil {
		t.Fatalf("published dispatch: %d %v", status, taken)
	}
}
