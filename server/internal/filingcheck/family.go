package filingcheck

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

func parent(d Record) string {
	if p := fleet.Text(d["parentSplit"]); p != "" {
		return p
	}
	return fleet.Text(d["parentConsultation"])
}
func rootID(d Record, byID map[string]Record) string {
	seen := map[string]int{}
	visited := []string{}
	for i := 0; i < 64; i++ {
		id := fleet.Text(d["id"])
		if position, exists := seen[id]; exists {
			cycle := append([]string{}, visited[position:]...)
			sort.Strings(cycle)
			return cycle[0]
		}
		seen[id] = len(visited)
		visited = append(visited, id)
		p := parent(d)
		if p == "" {
			return id
		}
		next := byID[p]
		if next == nil {
			return p
		}
		d = next
	}
	return ""
}
func familyMembers(d Record, drafts []Record) []Record {
	if parent(d) == "" {
		return nil
	}
	byID := map[string]Record{}
	for _, item := range drafts {
		byID[fleet.Text(item["id"])] = item
	}
	byID[fleet.Text(d["id"])] = d
	root := rootID(d, byID)
	if root == "" {
		return nil
	}
	out := []Record{}
	for _, item := range drafts {
		if rootID(item, byID) == root {
			out = append(out, item)
		}
	}
	return out
}
func paths(d Record) []string {
	out := []string{}
	for _, v := range fleet.Elements(obj(d["changeScope"])["paths"]) {
		out = append(out, fleet.Text(v))
	}
	normalized, err := jobs.NormalizeScope(out)
	if err != nil {
		return nil
	}
	return normalized
}

// reachableSiblings lists the siblings the draft depends on directly or transitively.
func reachableSiblings(d Record, byID map[string]Record) map[string]bool {
	dependencies := func(start Record) []string { return siblingDependencies(start, byID) }
	reachable := map[string]bool{}
	pending := dependencies(d)
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if reachable[id] {
			continue
		}
		reachable[id] = true
		pending = append(pending, dependencies(byID[id])...)
	}
	return reachable
}
func (s *Service) siblingOverlap(ctx context.Context, d Record, drafts []Record, presence Record) error {
	family := familyMembers(d, drafts)
	if len(family) == 0 {
		return nil
	}
	byID := map[string]Record{}
	position := map[string]int{}
	for i, other := range drafts {
		byID[fleet.Text(other["id"])] = other
		position[fleet.Text(other["id"])] = i
	}
	excluded := map[string]bool{fleet.Text(d["id"]): true}
	for id := parent(d); id != "" && !excluded[id]; id = parent(byID[id]) {
		excluded[id] = true
	}
	missing := map[string]bool{}
	for _, item := range fleet.Elements(presence["files"]) {
		f := obj(item)
		if f["exists"] == false {
			missing[fleet.Text(f["path"])] = true
		}
	}
	own := map[string]bool{}
	for _, path := range paths(d) {
		own[path] = true
	}
	reachable := reachableSiblings(d, byID)
	for _, other := range family {
		id := fleet.Text(other["id"])
		if excluded[id] || reachable[id] || !strings.EqualFold(fleet.Text(other["project"]), fleet.Text(d["project"])) {
			continue
		}
		filed := other["issue"] != nil || len(fleet.Elements(other["issues"])) > 0
		active := filed || other["status"] == "auto_ready" || other["status"] == "submitting" || other["status"] == "submit_unknown" || other["status"] == "submit_error" || other["status"] == "draft" && other["ready"] == true
		if !active {
			continue
		}
		earlier := filed || fleet.Text(other["createdAt"]) < fleet.Text(d["createdAt"]) || other["createdAt"] == d["createdAt"] && position[id] > position[fleet.Text(d["id"])]
		if !earlier {
			continue
		}
		for _, path := range paths(other) {
			if own[path] && missing[path] {
				return fail(fmt.Sprintf("起票前の確認を止めました: 先の相談 %s と新設ファイル %s が重なっています。", id, path))
			}
		}
	}
	return ctx.Err()
}
func siblingDependencies(d Record, byID map[string]Record) []string {
	out := []string{}
	p := byID[fleet.Text(d["parentSplit"])]
	children := fleet.Elements(p["splitChildren"])
	for i, id := range children {
		if fleet.Text(id) != fleet.Text(d["id"]) {
			continue
		}
		proposals := fleet.Elements(p["splitIssues"])
		if i < len(proposals) {
			for _, n := range fleet.Elements(obj(proposals[i])["dependsOn"]) {
				index := int(fleet.WholeNumber(n)) - 1
				if index >= 0 && index < len(children) {
					out = append(out, fleet.Text(children[index]))
				}
			}
		}
	}
	for id, candidate := range byID {
		for _, issue := range append([]any{candidate["issue"]}, fleet.Elements(candidate["issues"])...) {
			for _, dependency := range fleet.Elements(d["dependsOnIssues"]) {
				dep := obj(dependency)
				ref := obj(issue)
				if dep["source"] == "gitea" && fleet.WholeNumber(dep["number"]) == fleet.WholeNumber(ref["number"]) && strings.EqualFold(fleet.Text(ref["project"]), fleet.Text(d["project"])) {
					out = append(out, id)
				}
			}
		}
	}
	return out
}
func decisionContext(d Record, drafts []Record) Record {
	messages := []any{}
	for index, item := range fleet.Elements(d["messages"]) {
		m := obj(item)
		messages = append(messages, Record{"draftId": d["id"], "order": index + 1, "at": m["at"], "role": m["role"], "source": "conversation", "text": m["text"]})
	}
	refs := []any{}
	seen := map[string]bool{fleet.Text(d["id"]): true}
	byID := map[string]Record{}
	for _, item := range drafts {
		byID[fleet.Text(item["id"])] = item
	}
	for id := parent(d); id != "" && !seen[id] && len(refs) < 64; id = parent(byID[id]) {
		seen[id] = true
		if item := byID[id]; item != nil {
			refs = append(refs, Record{"draftId": id, "title": item["title"], "body": item["body"], "messages": item["messages"], "source": "ancestor"})
		} else {
			refs = append(refs, Record{"draftId": id, "unavailable": true})
			break
		}
	}
	return Record{"messages": messages, "references": refs, "omittedMessages": 0, "omittedReferences": 0}
}

var namedPath = regexp.MustCompile(`(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.[A-Za-z0-9]+|(?:AGENTS|README)\.md|(?:package|pyproject|go)\.(?:json|toml|mod)`)

func pathsFromBody(body string) []string { return namedPath.FindAllString(body, -1) }
func (s *Service) filePresence(ctx context.Context, d Record) (Record, error) {
	names := map[string]bool{}
	for _, name := range append(pathsFromBody(fleet.Text(d["title"])+"\n"+fleet.Text(d["body"])), paths(d)...) {
		if !strings.HasSuffix(name, "/") {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	repo := fleet.Text(d["project"])
	info, branch := map[string]any{}, map[string]any{}
	if err := s.options.Gitea.Do(ctx, "GET", "/repos/"+repo, nil, &info); err != nil {
		return nil, err
	}
	defaultBranch := fleet.Text(info["default_branch"])
	if defaultBranch == "" {
		return nil, errors.New("Giteaの既定ブランチを確認できません")
	}
	if err := s.options.Gitea.Do(ctx, "GET", "/repos/"+repo+"/branches/"+url.PathEscape(defaultBranch), nil, &branch); err != nil {
		return nil, err
	}
	revision := fleet.Text(obj(branch["commit"])["id"])
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) {
		return nil, errors.New("Giteaの固定版を確認できません")
	}
	entries := map[string]Record{}
	total := 0
	complete := false
	for page := 1; page <= 10; page++ {
		listing := map[string]any{}
		if err := s.options.Gitea.Do(ctx, "GET", fmt.Sprintf("/repos/%s/git/trees/%s?recursive=true&page=%d", repo, revision, page), nil, &listing); err != nil {
			return nil, err
		}
		tree := fleet.Elements(listing["tree"])
		total += len(tree)
		if tree == nil || total > 4000 {
			return nil, errors.New("Gitの木を完全に確認できません")
		}
		for _, item := range tree {
			entry := obj(item)
			entries[fleet.Text(entry["path"])] = entry
		}
		if listing["truncated"] != true {
			complete = true
			break
		}
	}
	if !complete {
		return nil, errors.New("Gitの木を完全に確認できません")
	}
	files := []any{}
	sorted := []string{}
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		entry, exists := entries[name]
		readable := exists && entry["type"] == "blob" && (entry["mode"] == "100644" || entry["mode"] == "100755") && consultationai.ContextFileAllowed(name)
		files = append(files, Record{"path": name, "exists": exists, "readable": readable})
	}
	return Record{"branch": defaultBranch, "revision": revision, "files": files}, nil
}
