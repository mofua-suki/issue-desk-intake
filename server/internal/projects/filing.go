package projects

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

func (s *Service) startFile(ctx context.Context, p Object) error {
	f := fleet.ObjectOrEmpty(p["filing"])
	if f["status"] == "filed" || s.running[fleet.Text(p["id"])] {
		return nil
	}
	if s.o.Submit == nil {
		return problem(503, "分割案の起票先が設定されていません。")
	}
	if p["status"] != "registered" || fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["registered"] == nil {
		return problem(409, "プロジェクトの登録が済んでいません。")
	}
	execution, release, e := s.acquire(ctx)
	if e != nil {
		return e
	}
	launched := false
	defer func() {
		if !launched {
			release()
		}
	}()
	f["status"] = "filing"
	f["error"] = nil
	if f["startedAt"] == nil {
		f["startedAt"] = s.now()
	}
	p["filing"] = f
	revision(p)
	if e := s.save(ctx, p); e != nil {
		return e
	}
	id := fleet.Text(p["id"])
	launched = true
	s.launchTask(id, "file", execution, func() error { return s.file(execution, id) }, release)
	return nil
}
func (s *Service) file(ctx context.Context, id string) error {
	s.mu.Lock()
	p, e := s.load(ctx, id)
	s.mu.Unlock()
	if e != nil {
		return e
	}
	d := fleet.ObjectOrEmpty(p["definition"])
	steps := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])
	full := fleet.Text(fleet.ObjectOrEmpty(steps["registered"])["name"])
	if full == "" || !approved(p) {
		return block("登録済みの定義への同意を確認できません。")
	}
	f := fleet.ObjectOrEmpty(p["filing"])
	if d["placement"] == "nas" && (f["host"] == nil || f["status"] == "host_waiting") {
		if s.o.Home == nil || s.o.Home(full) != "nas" {
			f["status"] = "host_waiting"
			f["waiting"] = "担当表へのNASの反映を待っています。"
			p["filing"] = f
			revision(p)
			s.mu.Lock()
			e = s.save(ctx, p)
			s.mu.Unlock()
			return e
		}
		f["host"] = Object{"at": s.now()}
		f["status"] = "filing"
		f["waiting"] = nil
	}
	registered := false
	if s.o.Repositories != nil {
		for _, r := range s.o.Repositories() {
			if strings.EqualFold(full, r) {
				registered = true
			}
		}
	}
	if !registered {
		return fmt.Errorf("registered project is not loaded")
	}
	if f["draft"] == nil {
		f["draft"] = uuid()
		p["filing"] = f
		s.mu.Lock()
		e = s.save(ctx, p)
		s.mu.Unlock()
		if e != nil {
			return e
		}
	}
	branch, commit, e := s.initialCommit(ctx, full, steps)
	if e != nil {
		return e
	}
	draft, e := s.filingDraft(p, d, id, full, branch, commit, f["draft"])
	if e != nil {
		return e
	}
	issues, e := s.o.Submit(ctx, draft)
	if e != nil {
		return e
	}
	return s.recordFiled(ctx, id, issues)
}

// initialCommit は初期ファイルのコミットを、記録に無ければ既定の枝から読み、枝とともに返す。
func (s *Service) initialCommit(ctx context.Context, full string, steps Object) (branch, commit string, err error) {
	branch = fleet.Text(fleet.ObjectOrEmpty(steps["created"])["branch"])
	if branch == "" {
		branch = "main"
	}
	commit = fleet.Text(fleet.ObjectOrEmpty(steps["committed"])["commit"])
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(commit) {
		if s.o.Gitea == nil {
			return "", "", problem(503, "初期コミットの照合に必要な管理用接続を利用できません。")
		}
		out := Object{}
		if e := s.o.Gitea.Do(ctx, "GET", "/repos/"+full+"/branches/"+branch, nil, &out); e != nil {
			return "", "", e
		}
		commit = fleet.Text(fleet.ObjectOrEmpty(out["commit"])["id"])
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(commit) {
			return "", "", fmt.Errorf("initial commit revision unavailable")
		}
	}
	return branch, commit, nil
}

// filingDraft は最初の分割案を起票する相談を、プロジェクト定義・同意・初期ファイルの指紋から組み立てる。
func (s *Service) filingDraft(p, d Object, id, full, branch, commit string, draftID any) (Object, error) {
	files, e := ProjectFiles(d, s.o.Owner)
	if e != nil {
		return nil, e
	}
	contextFiles := []any{}
	for _, f := range files {
		contextFiles = append(contextFiles, Object{"path": f.Path, "sha256": hash([]byte(f.Content))})
	}
	messages := []any{}
	for _, v := range fleet.List(p["messages"]) {
		m := fleet.ObjectOrEmpty(v)
		messages = append(messages, Object{"id": m["id"], "role": m["role"], "text": m["text"]})
	}
	body := strings.ReplaceAll(fmt.Sprintf("新しいプロジェクト %s の最初の分割案です。\n\n## プロジェクト定義\n%s\n\n## 利用者の同意\n%s\n\n## 実施順\n%s", full, jsonBytes(d), jsonBytes(p["approval"]), jsonBytes(d["splitIssues"])), "<!--", "&lt;!--")
	draft := Object{"id": draftID, "project": full, "provider": p["provider"], "projectDefinition": id, "status": "auto_ready", "ready": true, "questions": []any{}, "messages": messages, "title": full + " の最初の分割案", "body": body, "splitIssues": d["splitIssues"], "context": Object{"branch": branch, "revision": commit, "files": contextFiles}, "revision": 0, "createdAt": s.now()}
	return draft, nil
}

// recordFiled は起票した Issue を相談の記録に残し、起票を終えた状態にする。
func (s *Service) recordFiled(ctx context.Context, id string, issues []Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.load(ctx, id)
	if e != nil {
		return e
	}
	f := fleet.ObjectOrEmpty(p["filing"])
	f["status"] = "filed"
	f["error"] = nil
	f["issues"] = issues
	f["finishedAt"] = s.now()
	p["filing"] = f
	revision(p)
	return s.save(ctx, p)
}

// Run resumes durable operations and closes the service on caller cancellation.
func (s *Service) Run(ctx context.Context) error {
	return s.runRecovery(ctx, true, nil)
}
