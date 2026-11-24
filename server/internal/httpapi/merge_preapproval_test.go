package httpapi

import (
	"database/sql"
	"net/http"
	"testing"
	"time"
)

const preapprovalHead = "0123456789abcdef0123456789abcdef01234567"

// createdPreapprovedPull は承認付きの作成依頼を送り、監視がPR #3（head preapprovalHead）を作った結果まで進める。
func createdPreapprovedPull(t *testing.T, handler http.Handler, token, id, payload string) map[string]any {
	t.Helper()
	status, response := workerRequest(t, handler, token, "POST", "/api/v1/requests", `{"id":"`+id+`","kind":"pr_create","repo":"owner/repo","payload":`+payload+`}`)
	if status != 201 {
		t.Fatalf("create %d %v", status, response)
	}
	created := response["request"].(map[string]any)["payload"].(map[string]any)
	if status, response = workerRequest(t, handler, token, "GET", "/api/v1/requests/next?kind=pr_create&repo=owner/repo", ""); status != 200 || response["request"] == nil {
		t.Fatalf("claim %d %v", status, response)
	}
	if status, response = workerRequest(t, handler, token, "POST", "/api/v1/requests/"+id+"/result", `{"result":{"status":"created","number":3,"head":"`+preapprovalHead+`","url":"https://gitea.example/owner/repo/pulls/3"}}`); status != 200 {
		t.Fatalf("result %d %v", status, response)
	}
	return created
}

func preapprovedPayload() string {
	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	return `{"branch":"chat/test","title":"test","body":"test","approval":"直接PRでマージまで進めて","instructionAt":"` + at + `"}`
}

func mergeWithPreapproval(t *testing.T, handler http.Handler, token, id, head string) (int, map[string]any) {
	t.Helper()
	return workerRequest(t, handler, token, "POST", "/api/v1/requests", `{"id":"`+id+`","kind":"merge_manual","repo":"owner/repo","payload":{"number":3,"head":"`+head+`","approvalRequest":"1730000000100-0123abcd"}}`)
}

func TestMergeUsesPreapprovalFromCreateRequest(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, nil)
	created := createdPreapprovedPull(t, handler, token, "1730000000100-0123abcd", preapprovedPayload())
	if created["approvalDelegated"] != true || created["approval"] != "直接PRでマージまで進めて" {
		t.Fatalf("運用代行トークンの作成依頼に承認と印を残しません: %v", created)
	}
	status, response := mergeWithPreapproval(t, handler, token, "1730000000101-0123abcd", preapprovalHead)
	if status != 201 {
		t.Fatalf("merge %d %v", status, response)
	}
	payload := response["request"].(map[string]any)["payload"].(map[string]any)
	if payload["approval"] != "直接PRでマージまで進めて" || payload["approvalDelegated"] != true || payload["instructionAt"] != created["instructionAt"] || payload["approvalRequest"] != "1730000000100-0123abcd" {
		t.Fatalf("事前承認をマージ依頼へ写しません: %v", payload)
	}
	if status, _ = mergeWithPreapproval(t, handler, token, "1730000000101-0123abcd", preapprovalHead); status != 200 {
		t.Fatalf("同じ依頼の送り直しは同じ1件です: %d", status)
	}
}

func TestMergeRefusesUnmatchedPreapproval(t *testing.T) {
	handler, db, token := workerFixtureOptions(t, true, nil)
	createdPreapprovedPull(t, handler, token, "1730000000100-0123abcd", preapprovedPayload())
	if status, response := mergeWithPreapproval(t, handler, token, "1730000000102-0123abcd", "1123456789abcdef0123456789abcdef01234567"); status != 400 {
		t.Fatalf("作ったPRと違うheadを通しました: %d %v", status, response)
	}
	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	if status, response := workerRequest(t, handler, token, "POST", "/api/v1/requests", `{"id":"1730000000103-0123abcd","kind":"merge_manual","repo":"owner/repo","payload":{"number":3,"head":"`+preapprovalHead+`","approval":"PR 3 をマージして","instructionAt":"`+at+`","approvalRequest":"1730000000100-0123abcd"}}`); status != 400 {
		t.Fatalf("承認発言と事前承認の同時指定を通しました: %d %v", status, response)
	}
	if _, err := db.Exec("UPDATE requests SET sender='other' WHERE id='1730000000100-0123abcd'"); err != nil {
		t.Fatal(err)
	}
	if status, response := mergeWithPreapproval(t, handler, token, "1730000000104-0123abcd", preapprovalHead); status != 400 {
		t.Fatalf("別の送り手の作成依頼の承認を通しました: %d %v", status, response)
	}
	assertNoRequest(t, db, "1730000000104-0123abcd")
}

func TestMergeRefusesPreapprovalWithoutDelegation(t *testing.T) {
	handler, db, token := workerFixture(t)
	created := createdPreapprovedPull(t, handler, token, "1730000000100-0123abcd", preapprovedPayload())
	if _, has := created["approvalDelegated"]; has {
		t.Fatalf("運用代行を許していないトークンの作成依頼に印を付けました: %v", created)
	}
	if status, response := mergeWithPreapproval(t, handler, token, "1730000000105-0123abcd", preapprovalHead); status != 403 {
		t.Fatalf("運用代行でない承認を通しました: %d %v", status, response)
	}
	assertNoRequest(t, db, "1730000000105-0123abcd")
}

func TestMergeRefusesExpiredPreapproval(t *testing.T) {
	handler, db, token := workerFixtureOptions(t, true, nil)
	createdPreapprovedPull(t, handler, token, "1730000000100-0123abcd", preapprovedPayload())
	// 承認の指示から8日が過ぎた。
	old := time.Now().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE requests SET payload=jsonb_set(payload::jsonb, '{instructionAt}', to_jsonb($1::text)) WHERE id='1730000000100-0123abcd'`, old); err != nil {
		t.Fatal(err)
	}
	if status, response := mergeWithPreapproval(t, handler, token, "1730000000108-0123abcd", preapprovalHead); status != 400 {
		t.Fatalf("期限を過ぎた事前承認を通しました: %d %v", status, response)
	}
	assertNoRequest(t, db, "1730000000108-0123abcd")
}

func TestMergeRefusesCreateRequestWithoutApproval(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, nil)
	createdPreapprovedPull(t, handler, token, "1730000000100-0123abcd", `{"branch":"chat/test","title":"test","body":"test"}`)
	if status, response := mergeWithPreapproval(t, handler, token, "1730000000106-0123abcd", preapprovalHead); status != 400 {
		t.Fatalf("承認の無い作成依頼を事前承認として通しました: %d %v", status, response)
	}
}

func assertNoRequest(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM requests WHERE id=$1", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("断った依頼が残りました: count=%d err=%v", count, err)
	}
}
