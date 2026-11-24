package issueapi

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func deletionFixture(t *testing.T) (*fixture, string) {
	f := newFixture(t)
	head := strings.Repeat("a", 40)
	raw, _ := json.Marshal(Object{"deletionApproval": Object{"head": head, "items": []any{Object{"kind": "削除", "path": "old.txt"}}, "total": 1}})
	if _, e := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='deletions',pr=12,head_sha=$2,branch='work',view=$3::jsonb WHERE id=$1`, f.job, head, string(raw)); e != nil {
		t.Fatal(e)
	}
	return f, head
}

func TestDeletionApprovalByDelegatedTokenPostgres(t *testing.T) {
	f, head := deletionFixture(t)
	id := "77777777-7777-4777-8777-777777777777"
	statement := "共通番号1のold.txtの削除を承認してマージして<!-- x"
	data := Object{"id": id, "project": "owner/repo", "number": 1, "action": "approve-deletions", "head": strings.Repeat("c", 40), "statement": statement, "instructionAt": "2026-11-01T08:58:00+09:00"}
	if code, _ := f.call("POST", "/api/comment", "delegate", data); code != 409 {
		t.Fatal("different head accepted", code)
	}
	data = Object{"id": "99999999-9999-4999-8999-999999999999", "project": "owner/repo", "number": 1, "action": "approve-deletions", "head": head, "statement": statement, "instructionAt": "2026-11-01T08:58:00+09:00"}
	if code, _ := f.call("POST", "/api/comment", "nas", data); code != 403 {
		t.Fatal("token without delegation accepted", code)
	}
	data["id"] = id
	// 利用者がチャットで指示した日時の無い・未来の承認は断る。
	for _, at := range []any{nil, "2026-11-01T09:01:00+09:00"} {
		missing := Object{}
		for k, v := range data {
			missing[k] = v
		}
		missing["instructionAt"] = at
		if at == nil {
			delete(missing, "instructionAt")
		}
		if code, _ := f.call("POST", "/api/comment", "delegate", missing); code != 400 {
			t.Fatal("instructionAt", at, code)
		}
	}
	if code, value := f.call("POST", "/api/comment", "delegate", data); code != 200 {
		t.Fatal(code, value)
	}
	job, e := f.options.Jobs.Get(f.ctx, f.job)
	if e != nil || job.Status != "queued" || fleet.ObjectOrEmpty(job.View["deletionApproved"])["head"] != head || !strings.HasPrefix(fleet.Text(fleet.ObjectOrEmpty(job.View["deletionApproved"])["by"]), "token:") {
		t.Fatal("not approved", job, e)
	}
	if len(f.api.comments) != 2 {
		t.Fatal("audit comments", len(f.api.comments))
	}
	for _, target := range []int64{1, 12} {
		comments, err := f.api.Comments(f.ctx, "owner/repo", target)
		if err != nil || len(comments) != 1 {
			t.Fatal("IssueとPRそれぞれの監査記録が必要です", target, comments, err)
		}
	}
	audit := fmt.Sprint(f.api.comments)
	for _, want := range []string{"承認経路: token", "送信元PC:", "old.txtの削除を承認してマージして&lt;!-- x", "指示の日時: 2026-11-01T08:58:00+09:00", head} {
		if !strings.Contains(audit, want) {
			t.Fatal(want, audit)
		}
	}
	if code, _ := f.call("POST", "/api/comment", "delegate", data); code != 200 || len(f.api.comments) != 2 {
		t.Fatal("replay duplicated", code, len(f.api.comments))
	}
	data["statement"] = "共通番号1の別の削除を承認してマージして"
	if code, _ := f.call("POST", "/api/comment", "delegate", data); code != 409 {
		t.Fatal("changed statement accepted for the same id", code)
	}
}

func TestDeletionApprovalFromScreenKeepsRecordPostgres(t *testing.T) {
	f, head := deletionFixture(t)
	data := Object{"id": "88888888-8888-4888-8888-888888888888", "project": "owner/repo", "number": 1, "action": "approve-deletions", "sha": head}
	if code, value := f.call("POST", "/api/comment", "screen", data); code != 200 {
		t.Fatal(code, value)
	}
	if audit := fmt.Sprint(f.api.comments); !strings.Contains(audit, "受付画面の利用者") {
		t.Fatal(audit)
	}
}
