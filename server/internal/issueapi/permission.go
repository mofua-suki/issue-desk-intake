package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/releasepolicy"
	"reflect"
	"sort"
	"strings"
)

func sameStrings(a, b any) bool {
	aa, bb := []string{}, []string{}
	for _, v := range fleet.Items(a) {
		aa = append(aa, fleet.Text(v))
	}
	for _, v := range fleet.Items(b) {
		bb = append(bb, fleet.Text(v))
	}
	sort.Strings(aa)
	sort.Strings(bb)
	return reflect.DeepEqual(aa, bb)
}
func cleanPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n") {
		return false
	}
	for _, v := range strings.Split(p, "/") {
		if v == "" || v == "." || v == ".." {
			return false
		}
	}
	return true
}
func (s *Service) permissionTarget(ctx context.Context, issue Object, job *jobs.Job) (Object, error) {
	if job == nil || job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "review_held" || issue["state"] != "open" {
		return nil, nil
	}
	hold := fleet.ObjectOrEmpty(job.View["permissionHold"])
	candidate := len(hold) > 0 || strings.Contains(fleet.Text(job.Result["error"]), "権限の範囲を定めるファイルの変更")
	unavailable := func() Object {
		if candidate {
			return Object{"unavailable": "現在のPR・ジョブ・権限ファイルの完全な集合を照合できません。"}
		}
		return nil
	}
	if job.PR == nil || job.HeadSHA == nil || job.Branch == nil || !sha.MatchString(*job.HeadSHA) {
		return unavailable(), nil
	}
	route := fmt.Sprintf("/repos/%s/pulls/%d", job.Repo, *job.PR)
	pr := Object{}
	if e := s.o.Gitea.Do(ctx, "GET", route, nil, &pr); e != nil {
		return unavailable(), nil
	}
	match := func(p Object) bool {
		n, _ := number(p["number"])
		return n == *job.PR && p["state"] == "open" && p["merged"] == false && p["draft"] == false && fleet.ObjectOrEmpty(p["head"])["sha"] == *job.HeadSHA && fleet.ObjectOrEmpty(p["head"])["ref"] == *job.Branch && strings.EqualFold(fleet.Text(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["head"])["repo"])["full_name"]), job.Repo) && strings.EqualFold(fleet.Text(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["base"])["repo"])["full_name"]), job.Repo) && sha.MatchString(fleet.Text(fleet.ObjectOrEmpty(p["base"])["sha"]))
	}
	if !match(pr) {
		return unavailable(), nil
	}
	if len(hold) == 0 {
		hold = s.legacyPermissionHold(ctx, route, job)
		candidate = candidate || len(hold) > 0
	}
	allowed := map[string]bool{}
	for _, v := range releasepolicy.Files(job.Repo) {
		allowed[v] = true
	}
	if !validPermissionHold(hold, allowed) {
		return unavailable(), nil
	}
	rows, e := s.o.Gitea.Pages(ctx, route+"/files")
	if e != nil {
		return unavailable(), nil
	}
	total, ok := number(pr["changed_files"])
	if !ok || int64(len(rows)) != total {
		return unavailable(), nil
	}
	files, ok := changedPermissionFiles(rows, allowed)
	if !ok {
		return unavailable(), nil
	}
	if len(files) == 0 {
		return unavailable(), nil
	}
	refreshed := hold["head"] != *job.HeadSHA || hold["pr"] != float64(*job.PR) && fmt.Sprint(hold["pr"]) != fmt.Sprint(*job.PR)
	if !refreshed && !sameStrings(files, hold["files"]) {
		return unavailable(), nil
	}
	unchanged, e := s.permissionTargetUnchanged(ctx, route, job, pr, match)
	if e != nil {
		return nil, e
	}
	if !unchanged {
		return unavailable(), nil
	}
	return Object{"head": *job.HeadSHA, "pr": *job.PR, "files": files, "source": "structured", "project": job.Repo, "generation": job.Generation, "epoch": job.LeaseEpoch}, nil
}

// validPermissionHold は保存した権限変更の保留が今の形で、権限ファイルだけを重複なく挙げているかを確かめる。
func validPermissionHold(hold Object, allowed map[string]bool) bool {
	if num, ok := number(hold["v"]); !ok || num != 1 || !sha.MatchString(fleet.Text(hold["head"])) || !sha.MatchString(fleet.Text(hold["base"])) || len(fleet.Items(hold["files"])) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range fleet.Items(hold["files"]) {
		file := fleet.Text(v)
		if !allowed[file] || seen[file] {
			return false
		}
		seen[file] = true
	}
	return true
}

// changedPermissionFiles はPRの変更ファイルの一覧から、変更・改名・複製で触れた権限ファイルを並べて返す。一覧の形が不正なら ok=false を返す。
func changedPermissionFiles(rows []Object, allowed map[string]bool) ([]any, bool) {
	changed := map[string]bool{}
	permissions := map[string]bool{}
	for _, r := range rows {
		path, status := fleet.Text(r["filename"]), fleet.Text(r["status"])
		if !cleanPath(path) || changed[path] || !oneOf(status, "added", "changed", "modified", "deleted", "renamed", "copied", "type_changed") {
			return nil, false
		}
		changed[path] = true
		if allowed[path] {
			permissions[path] = true
		}
		previous := fleet.Text(r["previous_filename"])
		if status == "renamed" || status == "copied" {
			if !cleanPath(previous) {
				return nil, false
			}
			if allowed[previous] {
				permissions[previous] = true
			}
		} else if previous != "" {
			return nil, false
		}
	}
	files := []any{}
	names := []string{}
	for file := range permissions {
		names = append(names, file)
	}
	sort.Strings(names)
	for _, file := range names {
		files = append(files, file)
	}
	return files, true
}

// permissionTargetUnchanged は照合の間にPR・ジョブ・Issueが変わっていないかを読み直して確かめる。
func (s *Service) permissionTargetUnchanged(ctx context.Context, route string, job *jobs.Job, pr Object, match func(Object) bool) (bool, error) {
	again := Object{}
	if e := s.o.Gitea.Do(ctx, "GET", route, nil, &again); e != nil {
		return false, nil
	}
	current, e := s.o.Jobs.ByIssue(ctx, job.Repo, job.Issue)
	if e != nil {
		return false, e
	}
	currentIssue, e := s.o.Gitea.Issue(ctx, job.Repo, job.Issue)
	if e != nil {
		return false, nil
	}
	beforeJSON, _ := json.Marshal(job)
	afterJSON, _ := json.Marshal(current)
	if !match(again) || fleet.ObjectOrEmpty(again["base"])["sha"] != fleet.ObjectOrEmpty(pr["base"])["sha"] || fmt.Sprint(again["changed_files"]) != fmt.Sprint(pr["changed_files"]) || string(beforeJSON) != string(afterJSON) || currentIssue["state"] != "open" {
		return false, nil
	}
	return true, nil
}

func (s *Service) approvePermissions(ctx context.Context, repo string, n int64, data Object, p auth.Principal) (Object, error) {
	id, head, statement := fleet.Text(data["id"]), fleet.Text(data["head"]), strings.TrimSpace(fleet.Text(data["statement"]))
	if !uuid.MatchString(id) || !sha.MatchString(head) || !jobs.MeaningfulApproval(statement) || len(fleet.Items(data["images"])) > 0 {
		return nil, fail(400, "承認のID・SHA・具体的な利用者発言を確認してください。")
	}
	unlock := lock(&s.locks, "operation:"+id)
	defer unlock()
	issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
	defer issueUnlock()
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if job == nil {
		return nil, fail(409, "承認対象のジョブがありません。")
	}
	route := "screen"
	actor, pc, instructionAt := "受付画面の利用者", "", ""
	if p.Kind == auth.PC {
		route = "token"
		actor = ""
		pc = p.Name
		at, ok := contract.InstructionAt(data["instructionAt"], s.o.Now())
		if !ok {
			return nil, fail(400, contract.InstructionAtProblem)
		}
		instructionAt = at
	}
	identity := Object{"project": strings.ToLower(repo), "number": n, "head": head, "statement": statement, "actor": actor, "via": route, "pc": pc}
	if instructionAt != "" {
		identity["instructionAt"] = instructionAt
	}
	previous := fleet.ObjectOrEmpty(job.View["permissionApproval"])
	if previous["id"] == id && previous["resumePublication"] == true {
		if !sameJSON(previous["identity"], identity) {
			return nil, fail(409, "同じIDの承認内容が一致しません。")
		}
		return Object{"id": id, "head": previous["head"], "pr": previous["pr"], "replay": true}, nil
	}
	issue, e := s.o.Gitea.Issue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	target, e := s.permissionTarget(ctx, issue, job)
	if e != nil {
		return nil, e
	}
	if target == nil || target["unavailable"] != nil || target["head"] != head {
		return nil, fail(409, "現在の承認対象を照合できません。")
	}
	record := Object{"id": id, "identity": identity, "approvedBy": actor, "approvedFrom": route, "sourcePc": pc, "approvalStatement": statement}
	if instructionAt != "" {
		record["instructionAt"] = instructionAt
	}
	job, e = s.o.Jobs.PermissionApproval(ctx, job.ID, record, target, false)
	if e != nil {
		return nil, e
	}
	job, e = s.postPermissionAudit(ctx, repo, n, id, job)
	if e != nil {
		return nil, e
	}
	issue, e = s.o.Gitea.Issue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	target, e = s.permissionTarget(ctx, issue, job)
	if e != nil {
		return nil, e
	}
	if target == nil || target["unavailable"] != nil || target["head"] != head {
		return nil, fail(409, "監査投稿中に対象が変わったため公開を再開しません。")
	}
	job, e = s.o.Jobs.PermissionApproval(ctx, job.ID, record, target, true)
	if e != nil {
		return nil, e
	}
	s.emit(repo, n)
	return Object{"id": id, "head": head, "pr": fleet.ObjectOrEmpty(job.View["permissionApproval"])["pr"], "replay": false}, nil
}
func sameJSON(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

// postPermissionAudit は権限変更の承認を監査する投稿を Issue と PR へ1回ずつ行い、投稿の結果をジョブに記録する。結果を照合できない再投稿は断る。
func (s *Service) postPermissionAudit(ctx context.Context, repo string, n int64, id string, job *jobs.Job) (*jobs.Job, error) {
	for _, destination := range []string{"issue", "pr"} {
		saved := fleet.ObjectOrEmpty(job.View["permissionApproval"])
		if saved[destination+"Posted"] == true {
			continue
		}
		index := n
		if destination == "pr" {
			index, _ = number(saved["pr"])
		}
		body := permissionAudit(saved, id, destination)
		comments, e := s.o.Gitea.Comments(ctx, repo, index)
		if e != nil {
			return nil, e
		}
		var found any
		for _, comment := range comments {
			if s.o.Own(fleet.ObjectOrEmpty(comment["user"])) && comment["body"] == body {
				found = comment["id"]
				break
			}
		}
		if found == nil {
			if saved[destination+"Attempted"] == true {
				return nil, fail(503, "監査投稿の前回結果を照合できません。同じIDで確認を続けてください。")
			}
			job, e = s.o.Jobs.PermissionReceipt(ctx, job.ID, id, Object{destination + "Attempted": true})
			if e != nil {
				return nil, e
			}
			comment, e := s.o.Gitea.Comment(ctx, repo, index, body)
			if e != nil {
				if explicitRejection(e) {
					_, _ = s.o.Jobs.PermissionReceipt(ctx, job.ID, id, Object{destination + "Attempted": false})
				}
				return nil, e
			}
			found = comment["id"]
		}
		job, e = s.o.Jobs.PermissionReceipt(ctx, job.ID, id, Object{destination + "Posted": true, destination + "Comment": found})
		if e != nil {
			return nil, e
		}
	}
	return job, nil
}

func permissionAudit(record Object, id, destination string) string {
	files := []string{}
	for _, v := range fleet.Items(record["files"]) {
		files = append(files, "- "+fleet.Text(v))
	}
	// チャットの承認（トークン）だけ、利用者が指示した日時の行を足す（画面の承認と記録済みの承認の文は変えない）。
	instructed := ""
	if at := fleet.Text(record["instructionAt"]); at != "" {
		instructed = "\n指示の日時: " + at
	}
	return fmt.Sprintf("権限ファイル変更の承認記録\n承認経路: %s\n承認者: %s%s\n利用者の発言: %s%s\n対象コミット: %s\n対象PR: %v\n対象ファイル:\n%s\n\n<!-- issue-desk-permission-approval:%s:%s -->", fleet.Text(record["approvedFrom"]), fleet.Text(record["approvedBy"]), fleet.Text(record["sourcePc"]), strings.ReplaceAll(fleet.Text(record["approvalStatement"]), "<!--", "&lt;!--"), instructed, fleet.Text(record["head"]), record["pr"], strings.Join(files, "\n"), id, destination)
}
