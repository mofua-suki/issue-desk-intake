package httpapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/dispatch"
	"strings"
	"testing"
	"time"
)

func TestUsageUploadIsIdempotentAndRejectsContent(t *testing.T) {
	handler, db, token := workerFixture(t)
	body := `{"usage":[{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"worker","repo":"owner/repo","issue":1,"inputTokens":null,"outputTokens":0,"usageMissingReason":"usage-unavailable"},{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"worker","prompt":"secret"}],"logLines":[{"at":"2026-11-01T00:00:00Z","line":"finished"}]}`
	status, result := workerRequest(t, handler, token, "POST", "/api/v1/usage", body)
	if status != 200 || result["added"] != float64(2) || result["rejected"] != float64(1) {
		t.Fatalf("upload %d %v", status, result)
	}
	status, result = workerRequest(t, handler, token, "POST", "/api/v1/usage", body)
	if status != 200 || result["duplicate"] != float64(2) || result["rejected"] != float64(1) {
		t.Fatalf("repeat %d %v", status, result)
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/v1/usage", `{"usage":[],"limits":null}`)
	if status != 400 {
		t.Fatalf("invalid batch %d", status)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM usage_rows").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
}

func TestRequestClaimsAndCommandAcknowledgments(t *testing.T) {
	handler, db, token := workerFixture(t)
	body := `{"id":"1730000000000-0123abcd","kind":"pr_create","repo":"owner/repo","payload":{"branch":"chat/test","title":"test","body":"test"}}`
	status, response := workerRequest(t, handler, token, "POST", "/api/v1/requests", body)
	if status != 201 {
		t.Fatalf("submit %d %v", status, response)
	}
	status, response = workerRequest(t, handler, token, "POST", "/api/v1/requests", body)
	if status != 200 {
		t.Fatalf("resubmit %d %v", status, response)
	}
	status, response = workerRequest(t, handler, token, "GET", "/api/v1/requests/next?kind=pr_create&repo=owner/repo", "")
	if status != 200 || response["request"] == nil {
		t.Fatalf("claim %d %v", status, response)
	}
	status, response = workerRequest(t, handler, token, "POST", "/api/v1/requests/1730000000000-0123abcd/result", `{"result":{"status":"created","number":1,"url":"https://gitea.example/owner/repo/pulls/1"}}`)
	if status != 200 {
		t.Fatalf("result %d %v", status, response)
	}
	queue := dispatch.Queue{DB: db}
	command, err := queue.EnqueueCommand(context.Background(), dispatch.Command{ID: "1730000000001-0123abcd", Repo: "owner/repo", Issue: 1, Kind: "stop", TargetHost: "nas"})
	if err != nil {
		t.Fatal(err)
	}
	status, response = workerRequest(t, handler, token, "POST", "/api/v1/commands/"+command.ID+"/ack", `{"status":"done","result":{"ok":true}}`)
	if status != 200 {
		t.Fatalf("ack %d %v", status, response)
	}
	status, response = workerRequest(t, handler, token, "GET", "/api/v1/commands", "")
	if status != 200 || len(response["commands"].([]any)) != 0 {
		t.Fatalf("completed command resent %d %v", status, response)
	}
}

func TestMergeRequestIgnoresForgedDelegationMark(t *testing.T) {
	handler, _, token := workerFixture(t)
	instructionAt := time.Now().Add(-time.Minute).Format(time.RFC3339)
	body := `{"id":"1730000000002-0123abcd","kind":"merge_manual","repo":"owner/repo","payload":{"number":3,"head":"0123456789abcdef0123456789abcdef01234567","approval":"PR 3 をマージして","instructionAt":"` + instructionAt + `","approvalDelegated":true}}`
	status, response := workerRequest(t, handler, token, "POST", "/api/v1/requests", body)
	if status != 201 {
		t.Fatalf("submit %d %v", status, response)
	}
	if strings.Contains(fmt.Sprint(response["request"]), "approvalDelegated") {
		t.Fatalf("運用代行を許していないトークンの印を受け入れました: %v", response)
	}
}

func TestIntakeCallIDUploadDeduplicatesAndRejectsConflicts(t *testing.T) {
	handler, db, token := workerFixture(t)
	prefix := `{"usage":[{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"intake","provider":"codex","model":"fixture","callId":"`
	suffix := `"}]}`
	id1 := "11111111-1111-4111-8111-111111111111"
	id2 := "22222222-2222-4222-8222-222222222222"
	check := func(body string, key string) {
		t.Helper()
		status, response := workerRequest(t, handler, token, "POST", "/api/v1/usage", body)
		if status != 200 || response[key] != float64(1) {
			t.Fatalf("status=%d response=%v", status, response)
		}
	}
	check(prefix+id1+suffix, "added")
	check(prefix+id1+suffix, "duplicate")
	check(prefix+id2+suffix, "added")
	check(prefix+id1+`","outputTokens":7}]}`, "rejected")
	check(strings.Replace(prefix+id1+suffix, `"source":"intake"`, `"source":"worker"`, 1), "rejected")
	check(prefix+id1+`","prompt":"secret"}]}`, "rejected")
	var count int
	if err := db.QueryRow("SELECT count(*) FROM usage_rows").Scan(&count); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
