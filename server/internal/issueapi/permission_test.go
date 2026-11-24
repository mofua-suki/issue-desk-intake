package issueapi

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

type permissionAPI struct {
	*fakeAPI
	head, branch string
	files        []Object
	changed      int
	readCount    int
	changeAfter  int
}

func (p *permissionAPI) Do(ctx context.Context, method, route string, data, out any) error {
	if method == "GET" && route == "/repos/owner/repo/pulls/12" {
		p.readCount++
		head := p.head
		if p.changeAfter > 0 && p.readCount >= p.changeAfter {
			head = strings.Repeat("c", 40)
		}
		pr := Object{"number": 12, "state": "open", "merged": false, "draft": false, "changed_files": p.changed, "head": Object{"sha": head, "ref": p.branch, "repo": Object{"full_name": "owner/repo"}}, "base": Object{"sha": strings.Repeat("b", 40), "repo": Object{"full_name": "owner/repo"}}}
		raw, _ := json.Marshal(pr)
		return json.Unmarshal(raw, out)
	}
	return p.fakeAPI.Do(ctx, method, route, data, out)
}
func (p *permissionAPI) Pages(ctx context.Context, route string) ([]Object, error) {
	if route == "/repos/owner/repo/pulls/12/files" {
		return p.files, nil
	}
	return p.fakeAPI.Pages(ctx, route)
}
func permissionFixture(t *testing.T) (*fixture, *permissionAPI) {
	f := newFixture(t)
	head := strings.Repeat("a", 40)
	api := &permissionAPI{fakeAPI: f.api, head: head, branch: "work", changed: 1, files: []Object{{"filename": "host-ssh.json", "status": "modified"}}}
	hold := Object{"v": 1, "head": head, "base": strings.Repeat("b", 40), "pr": 12, "files": []any{"host-ssh.json"}}
	raw, _ := json.Marshal(Object{"permissionHold": hold})
	if _, e := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='review_held',pr=12,head_sha=$2,branch='work',view=$3::jsonb WHERE id=$1`, f.job, head, string(raw)); e != nil {
		t.Fatal(e)
	}
	f.options.Gitea = api
	f.restart()
	return f, api
}
func TestPermissionHTTPRequiresFullSetAuditAndHeadFencePostgres(t *testing.T) {
	f, api := permissionFixture(t)
	data := Object{"id": "66666666-6666-4666-8666-666666666666", "project": "owner/repo", "number": 1, "action": "approve-permissions", "head": api.head, "statement": "対象のhost-ssh.jsonの変更を利用者として承認します"}
	api.changed = 2
	code, _ := f.call("POST", "/api/comment", "screen", data)
	if code != 409 {
		t.Fatal("partial PR collection accepted", code)
	}
	api.changed = 1
	code, value := f.call("POST", "/api/comment", "screen", data)
	if code != 200 {
		t.Fatal(code, value)
	}
	job, e := f.options.Jobs.Get(f.ctx, f.job)
	if e != nil || job.Status != "queued" || fleet.ObjectOrEmpty(job.View["permissionApproval"])["resumePublication"] != true || len(f.api.comments) != 2 {
		t.Fatal("not audited before resume", job, e, len(f.api.comments))
	}
	code, value = f.call("POST", "/api/comment", "screen", data)
	if code != 200 || fleet.ObjectOrEmpty(value)["replay"] != true || len(f.api.comments) != 2 {
		t.Fatal("approval replay duplicated", code, value)
	}
	data["statement"] = "別の発言の内容で承認します"
	code, _ = f.call("POST", "/api/comment", "screen", data)
	if code != 409 {
		t.Fatal("same ID different statement", code)
	}
}
func TestPermissionHTTPHeadChangeDuringAuditStaysHeldPostgres(t *testing.T) {
	f, api := permissionFixture(t)
	api.changeAfter = 3
	data := Object{"id": "77777777-7777-4777-8777-777777777777", "project": "owner/repo", "number": 1, "action": "approve-permissions", "head": api.head, "statement": "対象のhost-ssh.jsonの変更を利用者として承認します"}
	// 利用者がチャットで指示した日時の無い承認は、監査も再開もせずに断る。
	if code, _ := f.call("POST", "/api/comment", "delegate", data); code != 400 || len(f.api.comments) != 0 {
		t.Fatal("approval without instructionAt", code, len(f.api.comments))
	}
	data["instructionAt"] = "2026-11-01T08:58:00+09:00"
	code, value := f.call("POST", "/api/comment", "delegate", data)
	if code != 409 {
		t.Fatal("changed head resumed", code, value)
	}
	job, e := f.options.Jobs.Get(f.ctx, f.job)
	if e != nil || job.Status != "held" || fleet.ObjectOrEmpty(job.View["permissionApproval"])["resumePublication"] == true {
		t.Fatal("approval fence bypass", job, e)
	}
}
