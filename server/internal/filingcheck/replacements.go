package filingcheck

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

func (s *Service) replacementSources(ctx context.Context, d Record, numbers map[int64]bool) ([]any, error) {
	out := []any{}
	for n := range numbers {
		issue, err := s.options.Gitea.Issue(ctx, fleet.Text(d["project"]), n)
		if err != nil {
			return nil, err
		}
		if fleet.WholeNumber(issue["number"]) != n || issue["pull_request"] != nil || issue["state"] != "open" && issue["state"] != "closed" {
			return nil, fail("置き換え元Issueの状態を確認できません")
		}
		job, err := s.options.Jobs.ByIssue(ctx, fleet.Text(d["project"]), n)
		if err != nil {
			return nil, err
		}
		if job == nil {
			return nil, fail("置き換え元のジョブを確認できません")
		}
		switch job.Status {
		case "queued", "held", "failed", "stopped", "merged", "closed":
		default:
			return nil, fail("置き換え元が実装中または公開中のため起票しません")
		}
		out = append(out, Record{"number": n, "project": fleet.Text(d["project"]), "candidateId": fmt.Sprint("request:", n), "usage": "置換元の確認専用。必要な節または行を evidenceRequests の selection.body で指定してください", "state": issue["state"], "jobStatus": job.Status, "title": issue["title"]})
	}
	return out, nil
}

func hasFilingComparison(material Record) bool {
	return len(fleet.Elements(material["requests"])) > 0 || len(fleet.Elements(material["jobs"])) > 0 || len(fleet.Elements(material["consultations"])) > 0 || len(fleet.Elements(material["replacements"])) > 0 || len(fleet.Elements(obj(material["target"])["dependsOnIssues"])) > 0
}

func replacementEvidenceSources(d, material Record) map[string]Record {
	out := map[string]Record{}
	numbers := replacementNumbers(d)
	for _, item := range fleet.Elements(material["replacements"]) {
		row := obj(item)
		n := fleet.WholeNumber(row["number"])
		if n > 0 && numbers[n] && fleet.Text(d["project"]) != "" && strings.EqualFold(fleet.Text(row["project"]), fleet.Text(d["project"])) {
			out[fmt.Sprint("request:", n)] = row
		}
	}
	return out
}

func (s *Service) replacementEvidence(ctx context.Context, d, source Record) (Record, string, error) {
	n := fleet.WholeNumber(source["number"])
	issue, err := s.options.Gitea.Issue(ctx, fleet.Text(d["project"]), n)
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil || fleet.WholeNumber(issue["number"]) != n || issue["pull_request"] != nil || issue["state"] != "open" && issue["state"] != "closed" {
		return nil, "置き換え元Issueを取得できません", nil
	}
	return issue, "", nil
}
func (s *Service) replacementCycle(ctx context.Context, d, target Record, sources map[int64]bool) error {
	if len(sources) == 0 {
		return nil
	}
	type ref struct {
		repo   string
		number int64
	}
	pending := []ref{}
	for _, item := range fleet.Elements(target["dependsOnIssues"]) {
		pending = append(pending, ref{fleet.Text(d["project"]), fleet.WholeNumber(obj(item)["number"])})
	}
	seen := map[string]bool{}
	for len(pending) > 0 {
		r := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		key := fmt.Sprint(strings.ToLower(r.repo), "#", r.number)
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(seen) > 10000 {
			return fail("置き換え後の依存関係を完全に確認できません")
		}
		if strings.EqualFold(r.repo, fleet.Text(d["project"])) && sources[r.number] {
			return fail("置き換え元が依存経路に含まれ、置き換え後に循環するため起票しません")
		}
		job, err := s.options.Jobs.ByIssue(ctx, r.repo, r.number)
		if err != nil {
			return err
		}
		if job == nil {
			return fail("置き換え後の依存経路のジョブを確認できません")
		}
		detail, err := s.options.Jobs.Detail(ctx, job.ID)
		if err != nil {
			return err
		}
		for _, value := range fleet.Elements(detail["dependsOn"]) {
			dep, err := s.options.Jobs.Get(ctx, fleet.WholeNumber(value))
			if err != nil {
				return err
			}
			pending = append(pending, ref{dep.Repo, dep.Issue})
		}
		for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
			for _, value := range fleet.Elements(job.View[field]) {
				v := obj(value)
				repo := fleet.Text(v["repo"])
				if repo == "" {
					repo = fleet.Text(v["project"])
				}
				if repo == "" {
					repo = job.Repo
				}
				n := fleet.WholeNumber(v["issue"])
				if n == 0 {
					n = fleet.WholeNumber(v["number"])
				}
				if n > 0 {
					pending = append(pending, ref{repo, n})
				}
			}
		}
	}
	return nil
}
