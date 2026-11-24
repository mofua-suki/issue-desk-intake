package filingcheck

import (
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

// reduceSiblingBodies removes unfiled sibling bodies when the input is too large.
func reduceSiblingBodies(material, dc Record) {
	rows := []any{}
	ids := []string{}
	for _, item := range fleet.Elements(dc["splitSiblings"]) {
		row := clone(obj(item))
		if body, ok := row["body"].(string); ok && body != "" {
			delete(row, "body")
			row["bodyOmitted"] = true
			ids = append(ids, "consultation:"+fleet.Text(row["consultationId"]))
		}
		rows = append(rows, row)
	}
	if len(ids) > 0 {
		dc["splitSiblings"] = rows
		addInputLimitOmission(material, "未起票兄弟本文", ids)
	}
}

// splitSiblings describes direct siblings in the order of the saved split plan.
func (s *Service) splitSiblings(d Record, drafts []Record, issues []map[string]any) []any {
	byID := map[string]Record{}
	for _, row := range drafts {
		byID[fleet.Text(row["id"])] = row
	}
	p := byID[fleet.Text(d["parentSplit"])]
	if p == nil {
		return nil
	}
	result := []any{}
	for i, child := range fleet.Elements(p["splitChildren"]) {
		id := fleet.Text(child)
		if id == fleet.Text(d["id"]) {
			continue
		}
		sibling := byID[id]
		row := Record{"proposalNumber": i + 1, "consultationId": id, "giteaNumber": nil, "commonNumber": nil, "title": nil, "state": nil}
		if sibling != nil {
			row["title"] = sibling["title"]
			for _, item := range append([]any{sibling["issue"]}, fleet.Elements(sibling["issues"])...) {
				ref := obj(item)
				n := fleet.WholeNumber(ref["number"])
				if n <= 0 || !strings.EqualFold(fleet.Text(ref["project"]), fleet.Text(d["project"])) {
					continue
				}
				row["giteaNumber"] = n
				if s.options.DeskNumber != nil {
					row["commonNumber"] = s.options.DeskNumber(fleet.Text(d["project"]), n)
				}
				for _, issue := range issues {
					if fleet.WholeNumber(issue["number"]) == n && issue["pull_request"] == nil {
						row["state"] = issue["state"]
						if issue["title"] != nil {
							row["title"] = issue["title"]
						}
						break
					}
				}
				break
			}
			if sibling["issue"] == nil && len(fleet.Elements(sibling["issues"])) == 0 {
				row["body"] = sibling["body"]
			}
		}
		result = append(result, row)
	}
	return result
}

// Only saved, same-project sibling references can turn split dependencies into
// explicit Gitea dependencies; cross-project dependencies use a different gate.
func mergeSiblingDependencies(target, d Record, family []Record) {
	known := map[int64]bool{}
	for _, member := range family {
		if fleet.Text(member["parentSplit"]) != fleet.Text(d["parentSplit"]) || fleet.Text(member["id"]) == fleet.Text(d["id"]) || fleet.Text(d["parentSplit"]) == "" {
			continue
		}
		for _, item := range append([]any{member["issue"]}, fleet.Elements(member["issues"])...) {
			ref := obj(item)
			n := fleet.WholeNumber(ref["number"])
			if n > 0 && strings.EqualFold(fleet.Text(ref["project"]), fleet.Text(d["project"])) {
				known[n] = true
			}
		}
	}
	seen := map[int64]bool{}
	merged := []any{}
	for _, item := range fleet.Elements(target["dependsOnIssues"]) {
		n := fleet.WholeNumber(obj(item)["number"])
		if n > 0 && seen[n] {
			continue
		}
		if n > 0 {
			seen[n] = true
		}
		merged = append(merged, item)
	}
	for _, item := range fleet.Elements(d["dependencies"]) {
		n := fleet.WholeNumber(item)
		if ref := obj(item); ref != nil {
			if !strings.EqualFold(fleet.Text(ref["repo"]), fleet.Text(d["project"])) {
				continue
			}
			n = fleet.WholeNumber(ref["number"])
		}
		if known[n] && !seen[n] {
			merged = append(merged, Record{"source": "gitea", "number": n})
			seen[n] = true
		}
	}
	for _, n := range declaredSiblingDependencies(d, family) {
		if !seen[n] {
			merged = append(merged, Record{"source": "gitea", "number": n})
			seen[n] = true
		}
	}
	target["dependsOnIssues"] = merged
}

var siblingProposalNumber = regexp.MustCompile(`分割案[ \t]*([1-9][0-9]*)(?:[（(][^）)\r\n]*[）)])?`)
var siblingDependencyList = regexp.MustCompile(`^依頼者の指定による依存[ \t]*[:：][ \t]*((?:分割案[ \t]*[1-9][0-9]*(?:[（(][^）)\r\n]*[）)])?[ \t]*(?:[、,][ \t]*|$))+)$`)

// addBodyDeclaredSiblings adds the proposal numbers that the body's dependency lines name,
// outside code blocks and the reference part.
func addBodyDeclaredSiblings(text string, declared map[int64]bool) {
	body, _ := splitReference(text)
	fence := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if fence == "" {
				fence = trimmed[:n]
			} else if strings.HasPrefix(trimmed, fence) && strings.TrimSpace(trimmed[n:]) == "" {
				fence = ""
			}
			continue
		}
		if fence != "" || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if match := siblingDependencyList.FindStringSubmatch(line); match != nil {
			for _, ref := range siblingProposalNumber.FindAllStringSubmatch(match[1], -1) {
				declared[numString(ref[1])] = true
			}
		}
	}
}

// Resolve only declarations about this child through the saved parent/child
// mapping. Proposal numbers, common numbers, and Gitea numbers are distinct.
// Body-only declarations are adopted only in the requester-designated format.
// Generated dependency lines are represented by the saved plan instead.
func declaredSiblingDependencies(d Record, family []Record) []int64 {
	byID := map[string]Record{}
	for _, member := range family {
		byID[fleet.Text(member["id"])] = member
	}
	p := byID[fleet.Text(d["parentSplit"])]
	children, proposals := fleet.Elements(p["splitChildren"]), fleet.Elements(p["splitIssues"])
	positions := map[string]int{}
	index := -1
	for i, child := range children {
		id := fleet.Text(child)
		if id == "" || positions[id] != 0 {
			return nil // An ambiguous mapping is not dependency proof.
		}
		positions[id] = i + 1
		if id == fleet.Text(d["id"]) {
			index = i
		}
	}
	if fleet.Text(d["id"]) == "" || fleet.Text(d["project"]) == "" || index < 0 || index >= len(proposals) {
		return nil
	}
	proposal := obj(proposals[index])
	if project := fleet.Text(proposal["project"]); project != "" && !strings.EqualFold(project, fleet.Text(d["project"])) {
		return nil
	}
	declared := map[int64]bool{}
	for _, item := range fleet.Elements(proposal["dependsOn"]) {
		declared[fleet.WholeNumber(item)] = true
	}
	addBodyDeclaredSiblings(fleet.Text(d["body"]), declared)
	out := []int64{}
	for i, child := range children {
		if i >= index || !declared[int64(i+1)] {
			continue
		}
		member := byID[fleet.Text(child)]
		if fleet.Text(member["parentSplit"]) != fleet.Text(d["parentSplit"]) || !strings.EqualFold(fleet.Text(member["project"]), fleet.Text(d["project"])) {
			continue
		}
		if project := fleet.Text(obj(proposals[i])["project"]); project != "" && !strings.EqualFold(project, fleet.Text(d["project"])) {
			continue
		}
		for _, item := range append([]any{member["issue"]}, fleet.Elements(member["issues"])...) {
			ref := obj(item)
			if n := fleet.WholeNumber(ref["number"]); n > 0 && strings.EqualFold(fleet.Text(ref["project"]), fleet.Text(d["project"])) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
