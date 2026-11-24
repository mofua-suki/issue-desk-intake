package startup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/dispatch"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/projects"
)

func projectMergeID(value map[string]any) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("project-registration:%s:%v:%s", fleet.Text(value["repo"]), value["number"], fleet.Text(value["head"]))))
	b := sum[:16]
	b[6] = (b[6] & 15) | 80
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func (a *App) registerProjects() error {
	values, e := a.Values()
	if e != nil {
		return e
	}
	owner := fleet.Text(fleet.Object(values["projectCreation"])["owner"])
	var admin *gitea.Client
	var projectAPI projects.Gitea
	if owner != "" {
		file := absolute(a.Config.ApplicationDir, fleet.Text(values["tokenFile"]))
		// 管理用資格情報が無いときも通常受付を起動し、作成だけを利用不可にする。
		// 受付専用アカウントを管理操作へ代用しない。
		if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
			admin, e = gitea.New(fleet.Text(values["giteaUrl"]), file, nil)
			if e != nil {
				return e
			}
			projectAPI = admin
		}
	}
	queue := &dispatch.Queue{DB: a.DB}
	registration := projects.NewRegistration(projects.RegistrationOptions{
		Gitea: projectAPI, Repo: "mofua-suki/issue-desk-intake", ConfigFile: a.Config.File, RuntimeDir: a.Config.RuntimeDir,
		ValidateConfig: a.ValidateConfig, Reload: a.Reload, Home: a.Home,
		SubmitMerge: func(ctx context.Context, value map[string]any) (string, error) {
			id := projectMergeID(value)
			_, _, e := queue.Submit(ctx, dispatch.Request{ID: id, Kind: "merge_manual", Repo: fleet.Text(value["repo"]), Payload: map[string]any{"number": value["number"], "head": value["head"]}, Sender: "intake"})
			return id, e
		},
		MergeResult: func(ctx context.Context, id string) (map[string]any, error) {
			request, e := queue.GetRequest(ctx, id)
			if e != nil || request == nil {
				return nil, e
			}
			if request.Status != "done" {
				return nil, nil
			}
			return request.Result, nil
		},
	})
	a.Projects, e = projects.Register(a.Mux, projects.Options{
		Acquire: a.acquireGate, AppDB: a.SQLite.DB(), Auth: a.Auth, Owner: owner, ConfigFile: a.Config.File, RuntimeDir: a.Config.RuntimeDir,
		Provider: fleet.Text(values["provider"]), Gitea: projectAPI, Repositories: a.Repositories,
		Answer: a.AI.Answer, Home: a.Home, DeskNumber: a.DeskNumber, Publish: a.Publish,
		Preflight: registration.Preflight, Registration: registration.Publish, Registered: registration.Registered,
		Submit: a.submitProjectDraft,
		Collaborators: func(placement string) []string {
			current, err := a.Values()
			if err != nil {
				return nil
			}
			candidates := []string{fleet.Text(current["intakeAccount"]), fleet.Text(fleet.Object(fleet.Object(current["assignments"])["workerAccounts"])[placement])}
			out := []string{}
			seen := map[string]bool{strings.ToLower(owner): true}
			for _, login := range candidates {
				key := strings.ToLower(login)
				if login != "" && !seen[key] {
					seen[key] = true
					out = append(out, login)
				}
			}
			return out
		},
		Webhook: func(ctx context.Context, repo string) (map[string]any, error) {
			return a.projectWebhook(ctx, admin, repo)
		},
	})
	if e == nil {
		registration.Attach(a.Projects)
	}
	return e
}

func (a *App) submitProjectDraft(ctx context.Context, draft map[string]any) ([]map[string]any, error) {
	id := fleet.Text(draft["id"])
	saved, e := a.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, old consultations.Record) (consultations.Record, error) {
		if old != nil {
			return old, nil
		}
		return consultations.Record(draft), nil
	})
	if e != nil {
		return nil, e
	}
	result, e := a.Filing.Submit(ctx, id, consultations.Record{"revision": saved["revision"]}, auth.Principal{Kind: auth.Screen})
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, raw := range fleet.List(result["issues"]) {
		out = append(out, fleet.Object(raw))
	}
	if issue := fleet.Object(result["issue"]); len(issue) > 0 {
		out = append(out, issue)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("プロジェクトの案の起票結果を確認できません")
	}
	return out, nil
}
