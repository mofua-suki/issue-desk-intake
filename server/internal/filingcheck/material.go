package filingcheck

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/instructionacceptance"
	"issue-desk-intake/server/internal/jobs"
	"strings"
)

func (s *Service) material(ctx context.Context, d Record) (Record, error) {
	if s.options.Material != nil {
		return s.options.Material(ctx, d)
	}
	drafts, err := s.materialDrafts(ctx, d)
	if err != nil {
		return nil, err
	}
	return s.materialWithDrafts(ctx, d, drafts)
}

// familyMembership returns the draft IDs of the split family and the issues it filed in repo.
func familyMembership(family []Record, repo string) (map[string]bool, map[int64]bool) {
	familyID := map[string]bool{}
	familyIssues := map[int64]bool{}
	for _, member := range family {
		familyID[fleet.Text(member["id"])] = true
		for _, issue := range append([]any{member["issue"]}, fleet.Elements(member["issues"])...) {
			entry := obj(issue)
			if strings.EqualFold(fleet.Text(entry["project"]), repo) {
				familyIssues[fleet.WholeNumber(entry["number"])] = true
			}
		}
	}
	return familyID, familyIssues
}

// replacementNumbers returns the issue numbers the draft replaces.
func replacementNumbers(d Record) map[int64]bool {
	replacements := map[int64]bool{}
	for _, item := range fleet.Elements(d["replaceIssues"]) {
		if n := fleet.WholeNumber(item); n > 0 {
			replacements[n] = true
		} else if n = fleet.WholeNumber(obj(item)["number"]); n > 0 {
			replacements[n] = true
		}
	}
	return replacements
}
func (s *Service) materialWithDrafts(ctx context.Context, d Record, drafts []Record) (Record, error) {
	repo := fleet.Text(d["project"])
	if err := s.options.Project(repo); err != nil {
		return nil, err
	}
	issues, err := s.options.Gitea.IssuesForFiling(ctx, repo)
	if err != nil {
		if filing.TemporaryGiteaRead(err) {
			return nil, fmt.Errorf("%w: %w", filing.ErrFilingReadUnavailable, err)
		}
		return nil, err
	}
	jobs, dependencies, err := s.options.Jobs.ForFiling(ctx, repo)
	if err != nil {
		return nil, err
	}
	family := familyMembers(d, drafts)
	familyID, familyIssues := familyMembership(family, repo)
	replacements := replacementNumbers(d)
	material := Record{"target": Record{"project": repo, "title": d["title"], "body": d["body"], "changeScope": d["changeScope"], "kind": d["kind"], "dependsOnIssues": []any{}, "decisionContext": decisionContext(d, drafts)}, "requests": []any{}, "jobs": []any{}, "consultations": []any{}, "replacements": []any{}}
	replacementRows, err := s.replacementSources(ctx, d, replacements)
	if err != nil {
		return nil, err
	}
	material["replacements"] = replacementRows
	target := obj(material["target"])
	if err := addSplitIssueSections(target, d); err != nil {
		return nil, err
	}

	obj(target["decisionContext"])["splitSiblings"] = s.splitSiblings(d, drafts, issues)
	if target["kind"] != "investigation" {
		target["kind"] = "implementation"
	}
	config, err := s.options.Config(ctx)
	if err != nil {
		return nil, err
	}
	settings := obj(config["assignments"])
	existing, accepted := openJobRows(issues, jobs, dependencies, settings)
	for _, issue := range issues {
		n := fleet.WholeNumber(issue["number"])
		if issue["pull_request"] != nil || issue["state"] != "open" || !s.options.Own(obj(issue["user"])) {
			continue
		}
		if replacements[n] {

			continue
		}
		if familyIssues[n] {
			continue
		}
		origin := filing.DraftID(fleet.Text(issue["body"]))
		if origin == fleet.Text(d["id"]) || familyID[origin] {
			continue
		}
		candidate := Record{"number": n, "title": issue["title"], "body": issue["body"], "state": issue["state"], "paths": pathsFromBody(fleet.Text(issue["body"])), "acceptsInstructions": true}
		for key, value := range accepted[n] {
			candidate[key] = value
		}
		if job := existing[n]; job != nil {
			candidate["job"] = withoutJobBody(job)
			delete(existing, n)
		}
		material["requests"] = append(fleet.Elements(material["requests"]), candidate)
	}
	for _, job := range jobs {
		if row := existing[job.Issue]; row != nil && !familyIssues[job.Issue] && !replacements[job.Issue] {
			for key, value := range accepted[job.Issue] {
				row[key] = value
			}
			material["jobs"] = append(fleet.Elements(material["jobs"]), row)
		}
	}
	for _, candidate := range consultationCandidates(d, drafts, familyID) {
		material["consultations"] = append(fleet.Elements(material["consultations"]), candidate)
	}
	deps := fleet.Elements(d["dependsOnIssues"])
	if s.options.ResolvePreceding != nil {
		resolved, err := s.options.ResolvePreceding(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, dep := range resolved {
			n := fleet.WholeNumber(dep["issue"])
			if n < 1 {
				n = fleet.WholeNumber(dep["number"])
			}
			target["dependsOnIssues"] = append(fleet.Elements(target["dependsOnIssues"]), Record{"source": "gitea", "number": n})
		}
	} else {
		target["dependsOnIssues"] = deps
	}
	mergeSiblingDependencies(target, d, family)
	if err := s.replacementCycle(ctx, d, target, replacements); err != nil {
		return nil, err
	}
	explicit, err := s.explicitDependencyStates(ctx, repo, fleet.Elements(target["dependsOnIssues"]))
	if err != nil {
		return nil, err
	}
	material["explicitDependencies"] = explicit
	presence, err := s.filePresence(ctx, d)
	if err != nil {
		return nil, err
	}
	material["filePresence"] = presence
	dependencyDraft := clone(d)
	dependencyDraft["dependsOnIssues"] = target["dependsOnIssues"]
	if err := s.siblingOverlap(ctx, dependencyDraft, drafts, presence); err != nil {
		return nil, err
	}
	return material, nil
}

// materialDrafts selects ancestors and candidates within one database snapshot.
func (s *Service) materialDrafts(ctx context.Context, d Record) ([]Record, error) {
	return readMaterialDrafts(ctx, s.options.Records, d)
}

type materialDraftReader interface {
	ReadFilingSnapshot(context.Context, func([]consultations.DraftFields) []string) ([]consultations.Record, error)
}

func readMaterialDrafts(ctx context.Context, reader materialDraftReader, d Record) ([]Record, error) {
	return reader.ReadFilingSnapshot(ctx, func(entries []consultations.DraftFields) []string {
		drafts := make([]Record, len(entries))
		byID := map[string]Record{}
		for i, entry := range entries {
			drafts[i] = entry.Record
			byID[fleet.Text(entry.Record["id"])] = entry.Record
		}
		family := map[string]bool{}
		for _, member := range familyMembers(d, drafts) {
			family[fleet.Text(member["id"])] = true
		}
		ancestors := map[string]bool{}
		// Match decisionContext's reference boundary. Candidates and unfiled
		// siblings below have independent reasons to retain their full records.
		for id := parent(d); id != "" && id != fleet.Text(d["id"]) && !ancestors[id] && len(ancestors) < 64; id = parent(byID[id]) {
			ancestors[id] = true
		}
		comparisonAt := materialComparisonAt(d, drafts)
		keys := []string{}
		for _, entry := range entries {
			row := entry.Record
			if ancestors[fleet.Text(row["id"])] || (fleet.Text(row["parentSplit"]) != "" && fleet.Text(row["parentSplit"]) == fleet.Text(d["parentSplit"]) && fleet.Text(row["id"]) != fleet.Text(d["id"]) && row["issue"] == nil && len(fleet.Elements(row["issues"])) == 0) || materialCandidate(d, row, family, comparisonAt) {
				keys = append(keys, entry.Key)
			}
		}
		return keys
	})
}

func materialComparisonAt(d Record, drafts []Record) string {
	comparisonAt := fleet.Text(d["createdAt"])
	if p := fleet.Text(d["parentSplit"]); p != "" {
		for _, row := range drafts {
			if row["id"] == p {
				return fleet.Text(row["createdAt"])
			}
		}
	}
	return comparisonAt
}

func materialCandidate(d, other Record, familyID map[string]bool, comparisonAt string) bool {
	id := fleet.Text(other["id"])
	created := fleet.Text(other["createdAt"])
	return id != fleet.Text(d["id"]) && !familyID[id] && strings.EqualFold(fleet.Text(other["project"]), fleet.Text(d["project"])) && other["issue"] == nil && len(fleet.Elements(other["issues"])) == 0 && other["parentSplit"] == nil && activeConsultation(other) && (created < comparisonAt || created == comparisonAt && id < fleet.Text(d["id"]))
}

func activeConsultation(d Record) bool {
	switch fleet.Text(d["status"]) {
	case "thinking", "draft", "auto_ready", "submitting", "submit_unknown", "submit_error", "intake_waiting":
		return true
	}
	return false
}

// withoutJobBody は Issue と結合したジョブから本文を外し、確定本文を優先する。
func withoutJobBody(job Record) Record {
	out := clone(job)
	delete(out, "body")
	return out
}

// explicitDependencyStates reads whether each declared preceding issue is satisfied.
func (s *Service) explicitDependencyStates(ctx context.Context, repo string, deps []any) ([]any, error) {
	explicit := []any{}
	for _, item := range deps {
		n := fleet.WholeNumber(obj(item)["number"])
		if n < 1 {
			return nil, fail("明示依存のGitea番号が不正です")
		}
		issue, err := s.options.Gitea.Issue(ctx, repo, n)
		if err != nil {
			if filing.TemporaryGiteaRead(err) {
				return nil, fmt.Errorf("%w: %w", filing.ErrFilingReadUnavailable, err)
			}
			return nil, err
		}
		job, err := s.options.Jobs.ByIssue(ctx, repo, n)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", filing.ErrFilingReadUnavailable, err)
		}
		state := Record{"candidateId": fmt.Sprint("request:", n), "number": n, "title": issue["title"], "state": issue["state"], "satisfied": false, "merged": nil, "reason": nil}
		if issue["pull_request"] != nil {
			pr := map[string]any{}
			if err := s.options.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr); err != nil {
				return nil, err
			}
			state["merged"] = pr["merged"]
			state["satisfied"] = (issue["state"] == "open" || issue["state"] == "closed") && pr["state"] == "closed" && pr["merged"] == true
		} else if job == nil {
			state["reason"] = "対応するジョブがありません"
		} else {
			state["jobStatus"] = job.Status
			state["satisfied"] = (issue["state"] == "open" || issue["state"] == "closed") && (job.Status == "merged" || job.Status == "closed")
		}
		if issue["state"] != "open" && issue["state"] != "closed" {
			state["reason"] = "Issueの状態を確認できません"
		}
		explicit = append(explicit, state)
	}
	return explicit, nil
}

// consultationCandidates lists the other consultations to compare, without their messages.
func consultationCandidates(d Record, drafts []Record, familyID map[string]bool) []Record {
	candidates := []Record{}
	comparisonAt := materialComparisonAt(d, drafts)
	for _, other := range drafts {
		if !materialCandidate(d, other, familyID, comparisonAt) {
			continue
		}
		candidate := clone(other)
		candidate["messageCount"] = len(fleet.Elements(other["messages"]))
		delete(candidate, "messages")
		candidate["acceptsInstructions"] = other["status"] != "submitting"
		candidates = append(candidates, candidate)
	}
	return candidates
}

// openJobRows reads the jobs of issues that are not closed, with whether each accepts
// further instructions.
func openJobRows(issues []map[string]any, filingJobs []*jobs.Job, dependencies map[int64][]int64, settings map[string]any) (map[int64]Record, map[int64]map[string]any) {
	existing := map[int64]Record{}
	accepted := map[int64]map[string]any{}
	closedIssues := map[int64]bool{}
	for _, issue := range issues {
		if issue["pull_request"] == nil && issue["state"] == "closed" {
			closedIssues[fleet.WholeNumber(issue["number"])] = true
		}
	}
	for _, job := range filingJobs {
		if closedIssues[job.Issue] {
			continue
		}
		accepted[job.Issue] = instructionacceptance.Check("open", "", job, nil, settings, true)
		existing[job.Issue] = Record{"issue": job.Issue, "title": job.Title, "status": job.Status, "scope": job.Scope, "dependsOn": dependencies[job.ID], "body": obj(job.View)["body"]}
	}
	return existing, accepted
}
