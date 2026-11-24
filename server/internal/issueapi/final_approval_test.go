package issueapi

import (
	"bytes"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAttentionDecisionResponseAuthorization(t *testing.T) {
	f := newFixture(t)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET status='held',hold_reason='review_held',head_sha=$2,view=jsonb_build_object('completionPlan',$3::jsonb) WHERE id=$1`, f.job, head, plan); err != nil {
		t.Fatal(err)
	}
	status, detail := f.call("GET", "/api/issue?project=owner/repo&number=1", "screen", nil)
	if status != 200 {
		t.Fatalf("detail: %d %v", status, detail)
	}
	pending := fleet.Items(fleet.ObjectOrEmpty(detail)["pendingFinalApprovals"])
	if len(pending) != 1 || fleet.ObjectOrEmpty(pending[0])["target"] != "配置" || fleet.ObjectOrEmpty(pending[0])["operation"] != "配置" || fleet.ObjectOrEmpty(pending[0])["condition"] != "差分確認" {
		t.Fatalf("pending: %v", detail)
	}
	for _, state := range []string{"held", "failed", "stopped"} {
		if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET status=$2,hold_reason=CASE WHEN $2='held' THEN 'review_held' ELSE NULL END WHERE id=$1`, f.job, state); err != nil {
			t.Fatal(err)
		}
		for _, route := range []string{"/api/issues", "/api/issue?project=owner/repo&number=1&view=summary"} {
			code, body := f.call("GET", route, "screen", nil)
			row := fleet.ObjectOrEmpty(body)
			if route == "/api/issues" {
				items := fleet.Items(body)
				if len(items) != 1 {
					t.Fatalf("list: %v", body)
				}
				row = fleet.ObjectOrEmpty(items[0])
			}
			wire, _ := json.Marshal(row["pendingFinalApprovals"])
			want, _ := json.Marshal(pending)
			if code != 200 || string(wire) != string(want) {
				t.Fatalf("%s lost approval ID/head/reason: %d %v", route, code, body)
			}
		}
	}
	fingerprint := fleet.Text(fleet.ObjectOrEmpty(pending[0])["fingerprint"])
	data := Object{"project": "owner/repo", "number": 1, "id": "00000000-0000-4000-8000-000000000001", "action": "approve-final", "approvalId": "deploy", "approvalFingerprint": fingerprint, "head": head, "statement": "質問「owner/repo#1 の承認 deploy を了承しますか？」への利用者の回答: A"}
	raw, _ := json.Marshal(data)
	if err := f.app.AddCLIKey(f.ctx, auth.Hash("single-use"), time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cli", "origin", "csrf"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/comment", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		if mode == "cli" {
			r.Header.Set("X-Desk-Key", "single-use")
		} else {
			r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
			r.Header.Set("X-CSRF-Token", "csrf")
		}
		if mode == "origin" {
			r.Header.Set("Origin", "http://other.example")
		}
		if mode == "csrf" {
			r.Header.Set("X-CSRF-Token", "bad")
		}
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("%s: %d", mode, w.Code)
		}
	}
	status, _ = f.call("POST", "/api/comment", "nas", data)
	if status != 403 {
		t.Fatalf("token without delegation: %d", status)
	}
	// 運用代行トークンは、承認IDを含む利用者の承認の発言がなければ断る（AIの判断や定型語では送れない）。
	for _, statement := range []string{"", "承認します", "質問「owner/repo#1 の別の承認を了承しますか？」への利用者の回答: A"} {
		denied := Object{}
		for k, v := range data {
			denied[k] = v
		}
		denied["statement"] = statement
		status, _ := f.call("POST", "/api/comment", "delegate", denied)
		if status != 400 {
			t.Fatalf("delegate statement %q: %d", statement, status)
		}
	}
	for _, key := range []string{"head", "approvalId", "statement", "approvalFingerprint"} {
		original := data[key]
		data[key] = ""
		if key == "head" {
			data[key] = strings.Repeat("b", 40)
		}
		if key == "approvalId" {
			data[key] = "wrong-id"
		}
		status, _ := f.call("POST", "/api/comment", "screen", data)
		if status < 400 || status >= 500 {
			t.Fatalf("invalid %s: %d", key, status)
		}
		data[key] = original
	}
	changedPlan := strings.Replace(plan, "差分確認", "別の差分を確認", 1)
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET view=jsonb_set(view,'{completionPlan}',$2::jsonb) WHERE id=$1`, f.job, changedPlan); err != nil {
		t.Fatal(err)
	}
	status, _ = f.call("POST", "/api/comment", "screen", data)
	if status != 409 {
		t.Fatalf("stale fingerprint: %d", status)
	}
	unchanged, err := f.options.Jobs.ByIssue(f.ctx, "owner/repo", 1)
	if err != nil || len(fleet.Items(unchanged.View["finalApprovals"])) != 0 {
		t.Fatalf("denied answers changed approvals: %v %v", unchanged, err)
	}
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET view=jsonb_set(view,'{completionPlan}',$2::jsonb) WHERE id=$1`, f.job, plan); err != nil {
		t.Fatal(err)
	}
	status, body := f.call("POST", "/api/comment", "screen", data)
	if status != 200 {
		t.Fatalf("screen: %d %v", status, body)
	}
	job, err := f.options.Jobs.ByIssue(f.ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	records := fleet.Items(job.View["finalApprovals"])
	if len(records) != 1 {
		t.Fatal(records)
	}
	status, summary := f.call("GET", "/api/issue?project=owner/repo&number=1&view=summary", "screen", nil)
	if status != 200 || len(fleet.Items(fleet.ObjectOrEmpty(summary)["pendingFinalApprovals"])) != 0 {
		t.Fatalf("resolved approval remains visible: %d %v", status, summary)
	}
	record := fleet.ObjectOrEmpty(records[0])
	if record["id"] != "deploy" || record["headSha"] != head || record["messageId"] != data["id"] || record["quote"] != data["statement"] || record["at"] == "" || record["via"] != "screen" || record["pc"] != nil {
		t.Fatal(record)
	}
	// 同じ送信IDの送り直しは記録を重ねない。
	status, _ = f.call("POST", "/api/comment", "screen", data)
	if status != 200 {
		t.Fatalf("replay: %d", status)
	}
	if job, err = f.options.Jobs.ByIssue(f.ctx, "owner/repo", 1); err != nil || len(fleet.Items(job.View["finalApprovals"])) != 1 {
		t.Fatalf("replay added a record: %v %v", job, err)
	}
}

func TestFinalApprovalFromDelegatedToken(t *testing.T) {
	f := newFixture(t)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deployment_both_sha","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET status='held',hold_reason='review_held',head_sha=$2,view=jsonb_build_object('completionPlan',$3::jsonb) WHERE id=$1`, f.job, head, plan); err != nil {
		t.Fatal(err)
	}
	status, detail := f.call("GET", "/api/issue?project=owner/repo&number=1", "delegate", nil)
	pending := fleet.Items(fleet.ObjectOrEmpty(detail)["pendingFinalApprovals"])
	if status != 200 || len(pending) != 1 || fleet.ObjectOrEmpty(pending[0])["id"] != "deployment_both_sha" {
		t.Fatalf("delegated detail lacks pending approvals: %d %v", status, detail)
	}
	statement := "質問「共通番号5951（owner/repo#1）の承認 deployment_both_sha（head SHA: " + head + "）を了承しますか？」への利用者の回答: A"
	// 承認IDを語として含まない（別のIDの一部だけ）・head SHAを含まない発言は、別の承認・別のheadへの流用として断る。
	for _, denied := range []string{
		"質問「共通番号5951の承認 deployment_both_sha_v2（head SHA: " + head + "）を了承しますか？」への利用者の回答: A",
		"質問「共通番号5951の承認 deployment_both_sha を了承しますか？」への利用者の回答: A",
		"質問「共通番号5951の承認 deployment_both_sha（head SHA: " + strings.Repeat("b", 40) + "）を了承しますか？」への利用者の回答: A",
	} {
		input := Object{"project": "owner/repo", "number": 1, "id": "00000000-0000-4000-8000-000000000003", "action": "approve-final", "approvalId": "deployment_both_sha", "approvalFingerprint": fleet.ObjectOrEmpty(pending[0])["fingerprint"], "head": head, "statement": denied}
		if status, _ := f.call("POST", "/api/comment", "delegate", input); status != 400 {
			t.Fatalf("reused statement %q: %d", denied, status)
		}
	}
	data := Object{"project": "owner/repo", "number": 1, "id": "00000000-0000-4000-8000-000000000002", "action": "approve-final", "approvalId": "deployment_both_sha", "approvalFingerprint": fleet.ObjectOrEmpty(pending[0])["fingerprint"], "head": head, "statement": statement}
	// 利用者がチャットで答えた日時（UTCオフセット付き・未来でなく7日以内）の無い承認は断る。
	for _, at := range []any{nil, "2026-11-01T08:58:00", "2026-11-01T09:01:00+09:00", "2026-10-24T08:58:00+09:00"} {
		if at != nil {
			data["instructionAt"] = at
		}
		if status, _ := f.call("POST", "/api/comment", "delegate", data); status != 400 {
			t.Fatalf("instructionAt %v: %d", at, status)
		}
	}
	data["instructionAt"] = "2026-11-01T08:58:00+09:00"
	status, body := f.call("POST", "/api/comment", "delegate", data)
	if status != 200 {
		t.Fatalf("delegate: %d %v", status, body)
	}
	job, err := f.options.Jobs.ByIssue(f.ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	records := fleet.Items(job.View["finalApprovals"])
	if len(records) != 1 {
		t.Fatal(records)
	}
	record := fleet.ObjectOrEmpty(records[0])
	if record["quote"] != statement || record["instructionAt"] != data["instructionAt"] || record["via"] != "token" || record["pc"] == "" || record["pc"] == nil {
		t.Fatal(record)
	}
	if pending, err := jobs.PendingFinalApprovals(job); err != nil || len(pending) != 0 {
		t.Fatalf("approval still pending: %v %v", pending, err)
	}
	// 記録後にheadが進んでも、同じ送信IDの送り直しは記録済みとして返し、記録を重ねない。指紋は不要。
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET head_sha=$2 WHERE id=$1`, f.job, strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	data["approvalFingerprint"] = ""
	if status, body := f.call("POST", "/api/comment", "delegate", data); status != 200 {
		t.Fatalf("replay after head moved: %d %v", status, body)
	}
	if job, err = f.options.Jobs.ByIssue(f.ctx, "owner/repo", 1); err != nil || len(fleet.Items(job.View["finalApprovals"])) != 1 {
		t.Fatalf("replay added a record: %v %v", job, err)
	}
}

func TestFinalApprovalTransferDetailHTTP(t *testing.T) {
	f := newFixture(t)
	from, to := strings.Repeat("a", 40), strings.Repeat("b", 40)
	fingerprint := "sha256:" + strings.Repeat("c", 64)
	history := []Object{{"id": "deploy", "fingerprint": "approval-fingerprint", "messageId": "message", "fromHead": from, "toHead": to, "diffFingerprint": fingerprint, "at": "2026-11-01T00:00:00Z"}, {"id": "merge", "fromHead": to, "toHead": from, "diffFingerprint": fingerprint, "at": "2026-11-01T01:00:00Z"}}
	raw, _ := json.Marshal(history)
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET view=jsonb_set(view,'{finalApprovalTransfers}',$2::jsonb) WHERE id=$1`, f.job, string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "&view=summary"} {
		status, body := f.call("GET", "/api/issue?project=owner/repo&number=1"+suffix, "screen", nil)
		records := fleet.Items(fleet.ObjectOrEmpty(body)["finalApprovalTransfers"])
		if status != 200 || len(records) != 2 {
			t.Fatalf("%s: %d %v", suffix, status, body)
		}
		entry := fleet.ObjectOrEmpty(records[0])
		if entry["fromHead"] != from || entry["toHead"] != to || entry["diffFingerprint"] != fingerprint || entry["at"] != "2026-11-01T00:00:00Z" || entry["messageId"] != nil {
			t.Fatalf("%s: %v", suffix, entry)
		}
	}
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET view=view-'finalApprovalTransfers' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "&view=summary"} {
		status, body := f.call("GET", "/api/issue?project=owner/repo&number=1"+suffix, "screen", nil)
		if status != 200 || len(fleet.Items(fleet.ObjectOrEmpty(body)["finalApprovalTransfers"])) != 0 {
			t.Fatalf("legacy %s: %d %v", suffix, status, body)
		}
	}
}
