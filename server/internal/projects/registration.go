package projects

import (
	"context"
	"encoding/base64"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"reflect"
	"regexp"
)

type RegistrationOptions struct {
	Gitea Gitea
	// Repo is the trusted repository containing assignments.json and fleet.json.
	Repo                   string
	ConfigFile, RuntimeDir string
	ValidateConfig         func(Object) error
	Reload                 func(Object) error
	SubmitMerge            func(context.Context, Object) (string, error)
	MergeResult            func(context.Context, string) (Object, error)
	Home                   func(string) string
	Format                 func([]DeskChange) ([]DeskChange, error)
}
type Registration struct {
	o RegistrationOptions
	s *Service
}

func NewRegistration(o RegistrationOptions) *Registration { return &Registration{o: o} }
func (r *Registration) Attach(s *Service)                 { r.s = s }
func (r *Registration) get(ctx context.Context, route string) (Object, error) {
	v := Object{}
	e := r.o.Gitea.Do(ctx, "GET", "/repos/"+r.o.Repo+route, nil, &v)
	if missing(e) {
		return nil, nil
	}
	return v, e
}
func (r *Registration) base(ctx context.Context) (string, error) {
	repo, e := r.get(ctx, "")
	if e != nil {
		return "", e
	}
	branch := fleet.Text(repo["default_branch"])
	if branch == "" {
		return "", fmt.Errorf("placement repository default branch unavailable")
	}
	return branch, nil
}
func (r *Registration) files(ctx context.Context, ref string, nas bool) (map[string]DeskFile, error) {
	names := []string{assignmentsFile, fleetFile}
	if nas {
		names = append(names, registryFile)
	}
	out := map[string]DeskFile{}
	for _, name := range names {
		v, e := r.get(ctx, "/contents/"+name+"?ref="+url.QueryEscape(ref))
		if e != nil {
			return nil, e
		}
		if v == nil {
			if name == registryFile {
				continue
			}
			return nil, block("担当表のファイルを確認できません。")
		}
		raw, e := base64.StdEncoding.DecodeString(fleet.Text(v["content"]))
		if e != nil {
			return nil, e
		}
		if fleet.Text(v["sha"]) == "" {
			return nil, fmt.Errorf("placement file SHA missing")
		}
		out[name] = DeskFile{string(raw), fleet.Text(v["sha"])}
	}
	return out, nil
}
func (r *Registration) plan(ctx context.Context, ref string, d, entry Object) ([]DeskChange, error) {
	files, e := r.files(ctx, ref, d["placement"] == "nas")
	if e != nil {
		return nil, e
	}
	changes, e := PlanDeskChange(files, entry, fleet.Text(d["placement"]))
	if e != nil {
		return nil, e
	}
	if r.o.Format != nil {
		formatted, e := r.o.Format(changes)
		if e != nil {
			return nil, e
		}
		if len(formatted) != len(changes) {
			return nil, fmt.Errorf("formatter changed file set")
		}
		for i, v := range formatted {
			before, e := parseObject(changes[i].Text)
			if e != nil {
				return nil, e
			}
			after, e := parseObject(v.Text)
			if e != nil || v.Path != changes[i].Path || !reflect.DeepEqual(before, after) {
				return nil, fmt.Errorf("formatter changed placement data")
			}
			formatted[i].SHA = changes[i].SHA
		}
		changes = formatted
	}
	return changes, nil
}
func (r *Registration) Preflight(ctx context.Context, d, entry Object) error {
	if r.o.Gitea == nil || r.o.Repo == "" || r.o.ConfigFile == "" || r.o.ValidateConfig == nil || r.o.SubmitMerge == nil || r.o.MergeResult == nil {
		return fmt.Errorf("placement registration configuration incomplete")
	}
	if e := r.checkLocal(entry); e != nil {
		return e
	}
	base, e := r.base(ctx)
	if e != nil {
		return e
	}
	_, e = r.plan(ctx, base, d, entry)
	return e
}
func (r *Registration) Publish(ctx context.Context, id string, d, entry Object) (bool, error) {
	if r.s == nil {
		return false, fmt.Errorf("placement registration service not attached")
	}
	nas := d["placement"] == "nas"
	pullStep, mergeStep, branchPrefix := "assignPull", "assignMerge", "project-assign-"
	if nas {
		pullStep = "pull"
		mergeStep = "mergeRequested"
		branchPrefix = "project-nas-"
	}
	snapshot, e := r.s.Snapshot(ctx, id)
	if e != nil {
		return false, e
	}
	steps := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(snapshot["creation"])["steps"])
	pull := fleet.ObjectOrEmpty(steps[pullStep])
	if len(pull) == 0 {
		if pull, e = r.preparePlacementPull(ctx, id, d, entry, pullStep, branchPrefix); e != nil {
			return false, e
		}
		if e = r.s.Mark(ctx, id, pullStep, pull); e != nil {
			return false, e
		}
	}
	if pull["skipped"] == true {
		return r.ready(entry, d), nil
	}
	pr, e := r.get(ctx, fmt.Sprintf("/pulls/%d", fleet.Int64(pull["number"])))
	if e != nil {
		return false, e
	}
	if pr == nil {
		return false, fmt.Errorf("placement pull disappeared")
	}
	if pr["merged"] == true {
		if e = r.s.Mark(ctx, id, map[bool]string{true: "merged", false: "assignMerge"}[nas], Object{"merged": true, "commit": pr["merge_commit_sha"]}); e != nil {
			return false, e
		}
		return r.ready(entry, d), nil
	}
	if pr["state"] != "open" {
		return false, block("担当表のPRがマージされずに閉じられています。")
	}
	snapshot, e = r.s.Snapshot(ctx, id)
	if e != nil {
		return false, e
	}
	request, e := r.requestPlacementMerge(ctx, id, pr, d, entry, snapshot, mergeStep)
	if e != nil {
		return false, e
	}
	if !nas {
		return true, nil
	}
	return r.awaitNASMerge(ctx, id, request, entry, d)
}

// preparePlacementPull は担当表へ追加するPRを用意する。既にあればそれを、変更が無ければ省いた印を、無ければ専用ブランチに書き込んでPRを作り、その記録を返す。
func (r *Registration) preparePlacementPull(ctx context.Context, id string, d, entry Object, pullStep, branchPrefix string) (Object, error) {
	var pull Object
	base, e := r.base(ctx)
	if e != nil {
		return nil, e
	}
	branch := branchPrefix + id
	existing, e := r.get(ctx, "/pulls/"+url.PathEscape(base)+"/"+url.PathEscape(branch))
	if e != nil {
		return nil, e
	}
	if existing != nil {
		pull = Object{"number": existing["number"], "head": fleet.ObjectOrEmpty(existing["head"])["sha"], "base": base, "branch": branch, "url": existing["html_url"]}
	} else {
		changes, e := r.plan(ctx, base, d, entry)
		if e != nil {
			return nil, e
		}
		if len(changes) == 0 {
			pull = Object{"skipped": true, "base": base, "branch": branch}
		} else {
			branchInfo, e := r.get(ctx, "/branches/"+url.PathEscape(branch))
			if e != nil {
				return nil, e
			}
			head := fleet.Text(fleet.ObjectOrEmpty(branchInfo["commit"])["id"])
			if branchInfo != nil {
				remaining, e := r.plan(ctx, branch, d, entry)
				if e != nil {
					return nil, e
				}
				if len(remaining) != 0 {
					return nil, block("専用ブランチの担当表が想定と一致しません。")
				}
			} else {
				if e = r.s.Mark(ctx, id, pullStep+"Requested", Object{"branch": branch}); e != nil {
					return nil, e
				}
				files := []Object{}
				for _, v := range changes {
					f := Object{"operation": "update", "path": v.Path, "sha": v.SHA, "content": base64.StdEncoding.EncodeToString([]byte(v.Text))}
					if v.SHA == "" {
						f["operation"] = "create"
						delete(f, "sha")
					}
					files = append(files, f)
				}
				result := Object{}
				if e = r.s.write(id, "POST", "/repos/"+r.o.Repo+"/contents", Object{"branch": base, "new_branch": branch, "message": "新しいプロジェクトを担当表へ追加する", "files": files}, &result); e != nil {
					return nil, e
				}
				head = fleet.Text(fleet.ObjectOrEmpty(result["commit"])["sha"])
			}
			result := Object{}
			if e = r.s.write(id, "POST", "/repos/"+r.o.Repo+"/pulls", Object{"base": base, "head": branch, "title": "新しいプロジェクト " + fleet.Text(entry["name"]) + " を " + fleet.Text(d["placement"]) + " の担当へ追加する", "body": "画面で承認したプロジェクト定義に基づき、担当表と実行設定に所定の項目だけを追加します。相談ID: " + id}, &result); e != nil {
				return nil, e
			}
			if fleet.Int64(result["number"]) < 1 {
				return nil, fmt.Errorf("placement pull identity missing")
			}
			pull = Object{"number": result["number"], "head": fleet.ObjectOrEmpty(result["head"])["sha"], "base": base, "branch": branch, "url": result["html_url"]}
			if pull["head"] == nil {
				pull["head"] = head
			}
		}
	}
	return pull, nil
}

// requestPlacementMerge は担当表のPRを検証してマージを依頼し、依頼のIDを記録する。依頼済みならそのIDを返す。
func (r *Registration) requestPlacementMerge(ctx context.Context, id string, pr, d, entry, snapshot Object, mergeStep string) (string, error) {
	var e error
	merge := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(snapshot["creation"])["steps"])[mergeStep])
	request := fleet.Text(merge["id"])
	if request == "" {
		if e = r.verify(ctx, pr, d, entry); e != nil {
			return "", e
		}
		if _, e = r.s.current(id); e != nil {
			return "", e
		}
		request, e = r.o.SubmitMerge(ctx, Object{"repo": r.o.Repo, "number": pr["number"], "head": fleet.ObjectOrEmpty(pr["head"])["sha"]})
		if e != nil {
			return "", e
		}
		if request == "" {
			return "", fmt.Errorf("merge request identity missing")
		}
		if e = r.s.Mark(ctx, id, mergeStep, Object{"id": request, "head": fleet.ObjectOrEmpty(pr["head"])["sha"]}); e != nil {
			return "", e
		}
	}
	return request, nil
}

// awaitNASMerge はNASのマージ結果を確かめ、マージ済みなら記録する。結果がまだ無ければ false を返す。
func (r *Registration) awaitNASMerge(ctx context.Context, id, request string, entry, d Object) (bool, error) {
	result, e := r.o.MergeResult(ctx, request)
	if e != nil {
		return false, e
	}
	if result == nil {
		return false, nil
	}
	if result["status"] != "merged" {
		return false, block("担当表のマージが完了していません。結果を確認してください。")
	}
	if e = r.s.Mark(ctx, id, "merged", Object{"commit": result["mergeCommit"]}); e != nil {
		return false, e
	}
	return r.ready(entry, d), nil
}

func (r *Registration) ready(entry, d Object) bool {
	return d["placement"] != "nas" || r.o.Home != nil && r.o.Home(fleet.Text(entry["name"])) == "nas"
}

var commitID = regexp.MustCompile(`^[a-f0-9]{40}$`)

func (r *Registration) verify(ctx context.Context, pr, d, entry Object) error {
	base, head := fleet.Text(pr["merge_base"]), fleet.Text(fleet.ObjectOrEmpty(pr["head"])["sha"])
	if !commitID.MatchString(base) || !commitID.MatchString(head) {
		return block("PRの基点と先頭を確認できません。")
	}
	changes, e := r.plan(ctx, base, d, entry)
	if e != nil {
		return e
	}
	var listed []Object
	if e = r.o.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d/files?limit=50", r.o.Repo, fleet.Int64(pr["number"])), nil, &listed); e != nil {
		return e
	}
	if len(listed) != len(changes) {
		return block("担当表PRに予定外のファイル変更があります。")
	}
	expected := map[string]DeskChange{}
	for _, v := range changes {
		expected[v.Path] = v
	}
	seen := map[string]bool{}
	current, e := r.files(ctx, head, d["placement"] == "nas")
	if e != nil {
		return e
	}
	for _, v := range listed {
		name := fleet.Text(v["filename"])
		want, ok := expected[name]
		status := fleet.Text(v["status"])
		if !ok || seen[name] || fleet.Text(v["previous_filename"]) != "" || want.SHA != "" && status != "changed" && status != "modified" || want.SHA == "" && status != "added" || current[name].Text != want.Text {
			return block("担当表PRの差分が所定の追加だけではありません。")
		}
		seen[name] = true
	}
	return nil
}
