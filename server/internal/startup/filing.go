package startup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/models"
)

func (a *App) filingConfig() (filing.BuildConfig, error) {
	c, err := a.Values()
	if err != nil {
		return filing.BuildConfig{}, err
	}
	return filing.BuildConfig{Config: c, Records: a.Records, Project: a.Project,
		ProviderSettings: func(ctx context.Context, p string, tier consultations.Record) (consultations.Record, error) {
			v, e := models.ImplementationSettings(c, p, fleet.Text(tier["level"]))
			return consultations.Record{"provider": v.Provider, "model": v.Model, "effort": v.Effort}, e
		},
		ProviderLabels: a.providerLabels, ResolvePreceding: a.resolvePreceding,
		DependencyCheckable: func(target, source string) bool {
			if target == source || c["assignments"] == nil {
				return true
			}
			assign := fleet.Object(c["assignments"])
			if _, ok := fleet.Object(assign["repositories"])[source]; ok {
				return true
			}
			hosts := map[string]bool{}
			if home := fleet.Text(fleet.Object(assign["repositories"])[target]); home != "" {
				hosts[home] = true
			}
			for _, host := range fleet.Strings(fleet.Object(assign["implementers"])[target]) {
				hosts[host] = true
			}
			return len(hosts) == 1 && hosts["nas"]
		}, CheckCurrent: a.checkCurrent,
	}, nil
}

func (a *App) providerLabels(ctx context.Context, repo, provider string) ([]int64, error) {
	if err := a.Project(repo); err != nil {
		return nil, err
	}
	rows, err := a.Gitea.Pages(ctx, "/repos/"+repo+"/labels")
	if err != nil && provider != "codex" && filing.TemporaryGiteaRead(err) {
		return nil, precedingUnavailable(err)
	}
	if err == nil {
		for _, v := range rows {
			if v["name"] == "provider/"+provider {
				if id, ok := positiveNumber(v["id"]); ok {
					return []int64{id}, nil
				}
				err = errors.New("実行先ラベルのIDが不正です")
				break
			}
		}
	}
	if err == nil {
		label := map[string]any{}
		err = a.Gitea.Do(ctx, "POST", "/repos/"+repo+"/labels", map[string]any{"name": "provider/" + provider, "color": "428bca", "description": "このIssueの実装を行うAI"}, &label)
		if id, ok := positiveNumber(label["id"]); err == nil && ok {
			return []int64{id}, nil
		}
	}
	if provider == "codex" {
		a.Log("実行先ラベルを用意できませんでした。本文の実行先指定を使用します。原因: " + contract.SafeCause(err))
		return []int64{}, nil
	}
	return nil, contract.NewProblem(502, "実行先のラベルを付けられないため起票を止めました。", "")
}

func (a *App) resolvePreceding(ctx context.Context, d consultations.Record) ([]consultations.Record, error) {
	repo := fleet.Text(d["project"])
	if err := a.Project(repo); err != nil {
		return nil, err
	}
	resolved := []consultations.Record{}
	for _, raw := range fleet.List(d["dependsOnIssues"]) {
		spec := fleet.Object(raw)
		if record, ok := raw.(consultations.Record); ok {
			spec = map[string]any(record)
		}
		n, valid := positiveNumber(spec["number"])
		if !valid {
			return nil, contract.NewProblem(409, "先行Issueの番号が不正です。", "")
		}
		var desk any
		if spec["source"] == "desk" {
			if err := a.numbering(ctx, false); err != nil {
				return nil, filingNumberReadError(err)
			}
			mapped, ok, err := a.Numbers.Issue(ctx, n)
			if err != nil {
				return nil, precedingUnavailable(err)
			}
			if !ok {
				if err = a.numbering(ctx, true); err != nil {
					return nil, filingNumberReadError(err)
				}
				mapped, ok, err = a.Numbers.Issue(ctx, n)
				if err != nil {
					return nil, precedingUnavailable(err)
				}
			}
			if !ok || mapped.Project != repo {
				return nil, contract.NewProblem(409, "先行Issueには同じプロジェクトの共通番号を指定してください。", "")
			}
			desk = n
			n = mapped.Number
		} else if spec["source"] != "gitea" {
			return nil, contract.NewProblem(409, "先行Issueの番号の種類を指定してください。", "")
		}
		issue, err := a.Gitea.Issue(ctx, repo, n)
		if err != nil {
			if filing.TemporaryGiteaRead(err) {
				return nil, precedingUnavailable(err)
			}
			return nil, err
		}
		if issue["pull_request"] != nil {
			return nil, contract.NewProblem(409, "先行にはPRではなくIssueを指定してください。", "")
		}
		if issue["state"] == "closed" {
			if err = a.precedingMerged(ctx, repo, n); err != nil {
				return nil, err
			}
		}
		if desk == nil {
			desk = a.DeskNumber(repo, n)
		}
		resolved = append(resolved, consultations.Record{"source": spec["source"], "number": spec["number"], "repo": repo, "issue": n, "gitea": n, "desk": desk})
	}
	return resolved, nil
}

var precedingMergeSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

func filingNumberReadError(err error) error {
	if filing.TemporaryGiteaRead(err) {
		return precedingUnavailable(err)
	}
	return err
}

func precedingUnavailable(err error) error {
	return fmt.Errorf("%w: %w", filing.ErrFilingReadUnavailable, err)
}

func (a *App) precedingMerged(ctx context.Context, repo string, n int64) error {
	refused := func() error {
		return contract.NewProblem(409, "閉じた先行IssueのPRが対象ブランチへマージされたことを確認できません。", "")
	}
	job, err := a.Jobs.ByIssue(ctx, repo, n)
	if err != nil {
		return precedingUnavailable(err)
	}
	if job == nil {
		return refused()
	}
	if job.PR != nil {
		merged, err := a.precedingPRMerged(ctx, repo, *job.PR, false)
		if err != nil || merged {
			return err
		}
	}
	if a.Operations == nil {
		return refused()
	}
	ops, err := a.Operations()
	if err != nil {
		return precedingUnavailable(err)
	}
	direct, err := ops.DirectMergedPredecessorPR(ctx, repo, n)
	if err != nil {
		return precedingUnavailable(err)
	}
	if direct < 1 {
		return refused()
	}
	merged, err := a.precedingPRMerged(ctx, repo, direct, true)
	if err != nil {
		return err
	}
	if !merged {
		return refused()
	}
	return nil
}

func (a *App) precedingPRMerged(ctx context.Context, repo string, number int64, direct bool) (bool, error) {
	pr := map[string]any{}
	if err := a.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &pr); err != nil {
		if filing.TemporaryGiteaRead(err) {
			return false, precedingUnavailable(err)
		}
		return false, nil
	}
	base := fleet.Object(pr["base"])
	if pr["merged"] != true || fleet.Text(fleet.Object(base["repo"])["full_name"]) != repo || direct && !precedingMergeSHA.MatchString(fleet.Text(pr["merge_commit_sha"])) {
		return false, nil
	}
	repository := map[string]any{}
	if err := a.Gitea.Do(ctx, "GET", "/repos/"+repo, nil, &repository); err != nil {
		if filing.TemporaryGiteaRead(err) {
			return false, precedingUnavailable(err)
		}
		return false, nil
	}
	return fleet.Text(repository["default_branch"]) != "" && base["ref"] == repository["default_branch"], nil
}

func (a *App) backfillScope(ctx context.Context, repo string, issue map[string]any, body string) error {
	n, valid := positiveNumber(issue["number"])
	if !valid {
		return errors.New("変更範囲を書き戻すIssueを確認できません")
	}
	current, err := a.Gitea.Issue(ctx, repo, n)
	if err != nil {
		return err
	}
	if !a.Own(fleet.Object(current["user"])) || current["body"] != issue["body"] || current["content_version"] != issue["content_version"] {
		return contract.NewProblem(409, "Issue本文の版が変わったため変更範囲を書き戻しません。", "")
	}
	return a.Gitea.Do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", repo, n), map[string]any{"body": body, "content_version": current["content_version"]}, nil)
}

func (a *App) changeTier(d consultations.Record, requested string, at time.Time) (consultations.Record, error) {
	c, err := a.Values()
	if err != nil {
		return nil, err
	}
	base := fleet.Object(fleet.Clone(d["tier"]))
	if base == nil {
		// A consultation saved without a tier takes the requested one as its start.
		base, err = models.InitialTier(c, map[string]any{"consultationProvider": d["consultationProvider"], "requestedTier": requested}, at)
		if err != nil {
			return nil, err
		}
	}
	// pending is only on records saved before the tier classifier was removed.
	if requested == "" || base["level"] == "fixed" || base["level"] == requested && base["pending"] != true {
		return consultations.Record(base), nil
	}
	next, err := models.InitialTier(c, map[string]any{"consultationProvider": d["consultationProvider"], "requestedTier": requested}, at)
	if err != nil {
		return nil, err
	}
	if next["level"] == "fixed" {
		return consultations.Record(base), nil
	}
	history := append([]any{}, fleet.List(base["history"])...)
	history = append(history, map[string]any{"from": base["level"], "to": requested, "reason": "依頼者の指定で相談の難易度を変更", "at": contract.ISO(at), "source": "user"})
	for _, key := range []string{"level", "model", "effort"} {
		base[key] = next[key]
	}
	base["history"] = history
	delete(base, "pending")
	return consultations.Record(base), nil
}

// checkCurrent is supplied by the independent filing validation service.
func (a *App) checkCurrent(ctx context.Context, d consultations.Record) error {
	if a.FilingCheck == nil {
		return contract.NewProblem(503, "起票前の照合を準備中です。依頼を保存したまま待機します。", "")
	}
	return a.FilingCheck(ctx, d)
}
