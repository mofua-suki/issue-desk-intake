package projects

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"net/url"
	"strings"
)

type blocked struct{ message string }

func (b *blocked) Error() string { return b.message }
func block(message string) error { return &blocked{message} }
func (s *Service) startCreate(ctx context.Context, p Object) error {
	if p["status"] == "registered" || s.running[fleet.Text(p["id"])] {
		return nil
	}
	if !approved(p) {
		return problem(409, "同意済みの定義がありません。")
	}
	if !s.creatable() {
		return problem(503, "リポジトリ作成の登録先が設定されていません。")
	}
	switch p["status"] {
	case "create_approved", "creating", "creation_failed", "creation_blocked":
	default:
		return problem(409, "現在リポジトリを作成できません。")
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
	c := fleet.ObjectOrEmpty(p["creation"])
	if c["fingerprint"] != nil && c["fingerprint"] != fleet.ObjectOrEmpty(p["approval"])["fingerprint"] {
		if fleet.ObjectOrEmpty(c["steps"])["requested"] != nil {
			return problem(409, "作成開始済みの定義が変更されています。")
		}
		c = Object{}
	}
	if c["steps"] == nil {
		c["steps"] = Object{}
	}
	if c["startedAt"] == nil {
		c["startedAt"] = s.now()
	}
	c["fingerprint"] = fleet.ObjectOrEmpty(p["approval"])["fingerprint"]
	c["error"] = nil
	c["reason"] = nil
	p["creation"] = c
	p["status"] = "creating"
	revision(p)
	if e := s.save(ctx, p); e != nil {
		return e
	}
	id := fleet.Text(p["id"])
	launched = true
	s.launchTask(id, "create", execution, func() error { return s.createRepository(execution, id) }, release)
	return nil
}
func (s *Service) current(id string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.load(s.ctx, id)
	if e != nil {
		return nil, e
	}
	if p["status"] != "creating" || !approved(p) || fleet.ObjectOrEmpty(p["creation"])["fingerprint"] != fleet.ObjectOrEmpty(p["approval"])["fingerprint"] {
		return nil, block("作成開始後に定義または同意が変更されたため停止しました。")
	}
	return p, nil
}

// Mark persists a reconciliable external-operation checkpoint before proceeding.
func (s *Service) Mark(ctx context.Context, id, step string, value Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.load(ctx, id)
	if e != nil {
		return e
	}
	if !approved(p) || fleet.ObjectOrEmpty(p["creation"])["fingerprint"] != fleet.ObjectOrEmpty(p["approval"])["fingerprint"] {
		return block("作成する定義への同意が一致しません。")
	}
	c := fleet.ObjectOrEmpty(p["creation"])
	steps := fleet.ObjectOrEmpty(c["steps"])
	value = clone(value)
	value["at"] = s.now()
	steps[step] = value
	c["steps"] = steps
	p["creation"] = c
	return s.save(ctx, p)
}
func (s *Service) Snapshot(ctx context.Context, id string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.load(ctx, id)
	if e != nil {
		return nil, e
	}
	return clone(p), nil
}
func missing(e error) bool { var ge *gitea.Error; return errors.As(e, &ge) && ge.Status == 404 }
func (s *Service) optional(ctx context.Context, route string) (Object, error) {
	out := Object{}
	e := s.o.Gitea.Do(ctx, "GET", route, nil, &out)
	if missing(e) {
		return nil, nil
	}
	return out, e
}
func (s *Service) write(id, method, route string, payload any, out any) error {
	if _, e := s.current(id); e != nil {
		return e
	}
	return s.o.Gitea.Do(s.ctx, method, route, payload, out)
}
func (s *Service) createRepository(ctx context.Context, id string) error {
	p, e := s.current(id)
	if e != nil {
		return e
	}
	d := fleet.ObjectOrEmpty(p["definition"])
	entry := registrationEntry(d, s.o.Owner)
	if e = s.o.Preflight(ctx, d, entry); e != nil {
		return block("プロジェクトの登録先設定を検証できないため、作成を停止しました。")
	}
	user := Object{}
	if e = s.o.Gitea.Do(ctx, "GET", "/user", nil, &user); e != nil {
		return e
	}
	if !strings.EqualFold(fleet.Text(user["login"]), s.o.Owner) {
		return block("管理用アカウントと作成先の所有者が一致しません。")
	}
	full := s.o.Owner + "/" + fleet.Text(d["name"])
	route := "/repos/" + full
	steps := func() (Object, error) {
		p, e := s.current(id)
		if e != nil {
			return nil, e
		}
		return fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"]), nil
	}
	st, e := steps()
	if e != nil {
		return e
	}
	repo, e := s.optional(ctx, route)
	if e != nil {
		return e
	}
	if st["created"] == nil {
		if e = s.createGiteaRepository(ctx, id, d, full, route, st, repo); e != nil {
			return e
		}
	} else if repo == nil || fleet.Int64(repo["id"]) != fleet.Int64(fleet.ObjectOrEmpty(st["created"])["id"]) {
		return block("作成したリポジトリの識別子が一致しません。")
	}
	st, e = steps()
	if e != nil {
		return e
	}
	branch := fleet.Text(fleet.ObjectOrEmpty(st["created"])["branch"])
	files, e := ProjectFiles(d, s.o.Owner)
	if e != nil {
		return e
	}
	if st["committed"] == nil {
		if e = s.commitInitialFiles(ctx, id, route, branch, files); e != nil {
			return e
		}
	}
	st, e = steps()
	if e != nil {
		return e
	}
	if st["collaborators"] == nil {
		if e = s.addCollaborators(ctx, id, d, route); e != nil {
			return e
		}
	}
	if st["webhook"] == nil && d["placement"] != "nas" && s.o.Webhook != nil {
		result, hookErr := s.o.Webhook(ctx, full)
		if hookErr != nil {
			result = Object{"status": "failed", "reason": "Webhookを設定できませんでした。監視の定期確認は継続します。"}
		}
		if e = s.Mark(ctx, id, "webhook", result); e != nil {
			return e
		}
	}

	ready, e := s.o.Registration(ctx, id, d, entry)
	if e != nil || !ready {
		return e
	}
	if d["placement"] == "nas" && (s.o.Home == nil || s.o.Home(full) != "nas") {
		return nil
	}
	if _, e = s.current(id); e != nil {
		return e
	}
	if e = s.o.Registered(ctx, entry); e != nil {
		return e
	}
	if e = s.Mark(ctx, id, "registered", Object{"name": full}); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e = s.load(ctx, id)
	if e != nil {
		return e
	}
	p["status"] = "registered"
	c := fleet.ObjectOrEmpty(p["creation"])
	c["finishedAt"] = s.now()
	p["filing"] = Object{"status": "filing", "startedAt": s.now()}
	revision(p)
	if e = s.save(ctx, p); e != nil {
		return e
	}
	return nil
}

// createGiteaRepository はGiteaに非公開のリポジトリを作り、その識別子と既定の枝を作成の記録に残す。同名のリポジトリ・作成記録・作成の競合では止める。
func (s *Service) createGiteaRepository(ctx context.Context, id string, d Object, full, route string, st, repo Object) error {
	var e error
	if repo != nil && st["requested"] == nil {
		return block("同じ名前のリポジトリが既に存在します。名前を見直してください。")
	}
	if repo == nil {
		s.mu.Lock()
		records, readErr := s.all(ctx)
		s.mu.Unlock()
		if readErr != nil {
			return readErr
		}
		for _, other := range records {
			if other["id"] != id && strings.EqualFold(fleet.Text(fleet.ObjectOrEmpty(other["definition"])["name"]), fleet.Text(d["name"])) && fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(other["creation"])["steps"])["requested"] != nil {
				return block("同じ名前の別の作成記録があるため停止しました。")
			}
		}
		if e = s.Mark(ctx, id, "requested", Object{"name": full}); e != nil {
			return e
		}
		out := Object{}
		e = s.write(id, "POST", "/user/repos", Object{"name": d["name"], "description": d["description"], "private": true, "auto_init": false, "default_branch": "main"}, &out)
		if e != nil {
			var ge *gitea.Error
			if errors.As(e, &ge) && ge.Status == 409 {
				s.mu.Lock()
				if p, loadErr := s.load(ctx, id); loadErr == nil {
					delete(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"]), "requested")
					contract.LogFailure("プロジェクト作成の競合の保存 "+id, s.save(ctx, p))
				}
				s.mu.Unlock()
				return block("同名リポジトリの作成が競合しました。別経路のリポジトリを引き継がず停止します。")
			}
			return e
		}
		repo, e = s.optional(ctx, route)
		if e != nil {
			return e
		}
	}
	if fleet.Int64(repo["id"]) < 1 {
		return fmt.Errorf("created repository identity not found")
	}
	branch := fleet.Text(repo["default_branch"])
	if branch == "" {
		branch = "main"
	}
	if e = s.Mark(ctx, id, "created", Object{"id": repo["id"], "branch": branch}); e != nil {
		return e
	}
	return nil
}

// commitInitialFiles は既定の枝が無ければ初期ファイルを1つのコミットで作り、そのコミットを作成の記録に残す。
func (s *Service) commitInitialFiles(ctx context.Context, id, route, branch string, files []ProjectFile) error {
	found, e := s.optional(ctx, route+"/branches/"+url.PathEscape(branch))
	if e != nil {
		return e
	}
	commit := fleet.Text(fleet.ObjectOrEmpty(found["commit"])["id"])
	if found == nil {
		contents := []Object{}
		for _, f := range files {
			contents = append(contents, Object{"operation": "create", "path": f.Path, "content": base64.StdEncoding.EncodeToString([]byte(f.Content))})
		}
		out := Object{}
		if e = s.write(id, "POST", route+"/contents", Object{"branch": branch, "message": "プロジェクトの初期ファイルを作成する", "files": contents}, &out); e != nil {
			return e
		}
		commit = fleet.Text(fleet.ObjectOrEmpty(out["commit"])["sha"])
	}
	if e = s.Mark(ctx, id, "committed", Object{"commit": commit}); e != nil {
		return e
	}
	return nil
}

// addCollaborators は配置先の作業用アカウントに書き込み権限を付け、付けた・見つからなかったアカウントを作成の記録に残す。
func (s *Service) addCollaborators(ctx context.Context, id string, d Object, route string) error {
	var e error
	accounts, absent := []any{}, []any{}
	seen := map[string]bool{}
	if s.o.Collaborators != nil {
		for _, name := range s.o.Collaborators(fleet.Text(d["placement"])) {
			key := strings.ToLower(name)
			if name == "" || strings.EqualFold(name, s.o.Owner) || seen[key] {
				continue
			}
			if !repoName.MatchString(name) {
				return fmt.Errorf("invalid collaborator account")
			}
			seen[key] = true
			found, e := s.optional(ctx, "/users/"+url.PathEscape(name))
			if e != nil {
				return e
			}
			if found == nil {
				absent = append(absent, name)
				continue
			}
			e = s.write(id, "PUT", route+"/collaborators/"+url.PathEscape(name), Object{"permission": "write"}, nil)
			if e != nil {
				var ge *gitea.Error
				if errors.As(e, &ge) && ge.Status == 422 {
					absent = append(absent, name)
					continue
				}
				return e
			}
			accounts = append(accounts, name)
		}
	}
	if e = s.Mark(ctx, id, "collaborators", Object{"accounts": accounts, "missing": absent}); e != nil {
		return e
	}
	return nil
}
