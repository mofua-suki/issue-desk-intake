package httpapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/workplan"
)

// Workers send the fixed-test proof on the state route; the route must pass it
// to the job store instead of rejecting the field.
func TestStateRouteAcceptsControllerProof(t *testing.T) {
	handler, db, token := workerFixtureOptions(t, false, nil)
	if status, _ := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2}`); status != 200 {
		t.Fatal(status)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('owner/repo',1,'queued') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if status, taken := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"capacity":2}`); status != 200 || taken["job"] == nil {
		t.Fatal(status, taken)
	}
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "one", Criterion: "受入条件の全文", Owner: "implementer", Phase: "verification", Method: "対象版を照合", Expected: "一致", EvidenceKind: "fixed_test", Capabilities: []string{}, Inputs: []string{}, Children: []int{}}}}
	head := strings.Repeat("a", 40)
	view, _ := json.Marshal(map[string]any{"executionPlan": plan.Record()})
	if _, err := db.Exec(`UPDATE jobs SET head_sha=$2,view=view||$3::jsonb WHERE id=$1`, id, head, string(view)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../jobs/testdata/controller-test.json")
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]any{"planSHA": plan.Fingerprint(), "test": json.RawMessage(raw)})
	path := "/api/v1/jobs/" + fmtInt(id) + "/state"
	status, result := workerRequest(t, handler, token, "POST", path, `{"epoch":1,"status":"running","headSha":"`+head+`","controllerTest":`+string(proof)+`}`)
	if status != 200 {
		t.Fatal(status, result)
	}
	job, _ := result["job"].(map[string]any)
	stored, _ := job["view"].(map[string]any)
	evidence, err := workplan.DecodeEvidence(stored["executionReceipts"])
	if err != nil || len(evidence) != 1 || evidence[0].Requirement != "one" || evidence[0].Head != head {
		t.Fatal(evidence, err)
	}
	// A report proof that cannot be matched reaches the store and is refused there, not by the route.
	status, result = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"status":"running","headSha":"`+head+`","controllerReport":{"planSHA":"other","head":"`+head+`","marker":"x","bodySHA":"y"}}`)
	if status != 409 || result["code"] != "execution_report_invalid" {
		t.Fatal(status, result)
	}
	if status, _ = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"status":"running","unknownField":true}`); status != 400 {
		t.Fatal("unknown field", status)
	}
}
