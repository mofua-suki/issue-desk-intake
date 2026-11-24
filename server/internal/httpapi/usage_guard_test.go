package httpapi

import (
	"strings"
	"testing"
)

func TestAutomaticUsageChecksFenceDeduplicateAndConsumeExplicitAdmission(t *testing.T) {
	handler, db, token := workerFixture(t)
	if _, err := db.Exec(`INSERT INTO jobs(repo,issue,title,status,lease_host,lease_epoch,lease_expires_at,view)VALUES('owner/repo',7,'guard','running','nas',1,now()+interval '1 hour','{}')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow("SELECT id FROM jobs WHERE issue=7").Scan(&id); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/jobs/" + fmtInt(id) + "/events"
	status, response := workerRequest(t, handler, token, "POST", path, `{"epoch":1,"kind":"implementation_usage_check","data":{"rows":[],"skipped":0}}`)
	if status != 200 || response["usageLimitReason"] != nil {
		t.Fatalf("first run without usage stopped %d %v", status, response)
	}
	// The first implementation has no earlier usage. Missing measurements must
	// stop a later implementation; use its real generation, not a first-run job.
	if _, err := db.Exec(`UPDATE jobs SET generation=1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	status, response = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"kind":"implementation_usage_check","data":{"rows":[],"skipped":0}}`)
	if status != 200 || response["usageLimitReason"] == nil {
		t.Fatalf("unknown admitted %d %v", status, response)
	}
	row := `{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"worker","repo":"owner/repo","issue":7,"provider":"codex","step":"実装","inputTokens":5000000,"cacheReadTokens":0,"outputTokens":0}`
	status, response = workerRequest(t, handler, token, "POST", "/api/v1/usage", `{"usage":[`+row+`]}`)
	if status != 200 || response["added"] != float64(1) {
		t.Fatalf("upload %d %v", status, response)
	}
	status, response = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"kind":"implementation_usage_check","data":{"rows":[`+row+`],"skipped":0}}`)
	if status != 200 || response["usageLimitReason"] != nil {
		t.Fatalf("duplicate counted twice %d %v", status, response)
	}
	over := strings.Replace(row, "5000000", "9000000", 1)
	status, response = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"kind":"implementation_usage_check","data":{"rows":[`+over+`],"skipped":0}}`)
	if status != 200 || response["usageLimitReason"] == nil {
		t.Fatalf("over limit admitted %d %v", status, response)
	}
	if _, err := db.Exec(`UPDATE jobs SET view='{"reimplementExpected":{"generation":1,"usageAdmission":true}}'::jsonb WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	admission := `{"epoch":1,"kind":"implementation_usage_check","data":{"rows":[],"skipped":1,"admissionId":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}`
	status, response = workerRequest(t, handler, token, "POST", path, admission)
	if status != 200 || response["usageLimitReason"] != nil {
		t.Fatalf("explicit admission %d %v", status, response)
	}
	changed := strings.Replace(admission, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", 1)
	status, response = workerRequest(t, handler, token, "POST", path, changed)
	if status != 200 || response["usageLimitReason"] == nil {
		t.Fatalf("reused admission %d %v", status, response)
	}
	status, response = workerRequest(t, handler, token, "POST", path, strings.Replace(admission, `"epoch":1`, `"epoch":2`, 1))
	if status != 409 || response["code"] != "lease_lost" {
		t.Fatalf("stale fence %d %v", status, response)
	}
}
