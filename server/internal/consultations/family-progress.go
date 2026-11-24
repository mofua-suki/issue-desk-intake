package consultations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strconv"
	"strings"
)

// FamilyJob is a read-only projection supplied by the application.
type FamilyJob struct {
	Project       string
	Number        int64
	Desk          any
	Status        string
	Reason        string
	Waiting       []string
	PendingChecks []string
}
type FamilyIssue struct {
	Project string
	Number  int64
}
type FamilyJobs func(context.Context, []FamilyIssue) ([]FamilyJob, error)

// FamilyIssues returns issue states by repository and issue number.
type FamilyIssues func(context.Context, string) (map[string]string, error)

func familyKey(project string, number any) string {
	return strings.ToLower(fmt.Sprintf("%s#%v", project, number))
}

// FamilyProgress attaches derived data to copies, never to stored records.
func (s *Service) FamilyProgress(ctx context.Context, records []Record) []Record {
	out, byID, parents := familyProgressCopies(records)
	if !parents || s.options.FamilyJobs == nil {
		return out
	}
	jobs, err := s.options.FamilyJobs(ctx, familyChildIssues(out, byID))
	if err != nil {
		return out
	}
	byIssue := map[string]FamilyJob{}
	for _, j := range jobs {
		byIssue[familyKey(j.Project, j.Number)] = j
	}
	issueStates := map[string]map[string]string{}
	for _, p := range out {
		if p["parentSplit"] != nil || p["splitChildren"] == nil {
			continue
		}
		p["familyProgress"] = s.parentFamilyProgress(ctx, p, byID, byIssue, issueStates)
	}
	return out
}

// familyProgressCopies copies the records without derived progress, indexes them by ID and
// reports whether any is a split parent.
func familyProgressCopies(records []Record) ([]Record, map[string]Record, bool) {
	out := make([]Record, len(records))
	parents := false
	byID := map[string]Record{}
	for i, d := range records {
		c := Record{}
		for k, v := range d {
			if k != "familyProgress" {
				c[k] = v
			}
		}
		out[i] = c
		byID[text(c["id"])] = c
		if c["parentSplit"] == nil && c["splitChildren"] != nil {
			parents = true
		}
	}
	return out, byID, parents
}

// familyChildIssues lists the issues of the split children whose jobs the progress needs.
func familyChildIssues(out []Record, byID map[string]Record) []FamilyIssue {
	// Collect exactly the children examined by the aggregation below. Parsing the
	// formatted number may select extra jobs; familyKey remains the final match.
	issues := []FamilyIssue{}
	seen := map[FamilyIssue]bool{}
	for _, p := range out {
		if p["parentSplit"] != nil || p["splitChildren"] == nil {
			continue
		}
		for _, id := range fleet.Items(p["splitChildren"]) {
			c := byID[text(id)]
			if c == nil {
				continue
			}
			issue := record(c["issue"])
			project := text(c["project"])
			if text(issue["project"]) != "" {
				project = text(issue["project"])
			}
			n, err := strconv.ParseInt(fmt.Sprint(issue["number"]), 10, 64)
			if err != nil {
				continue
			}
			key := FamilyIssue{Project: project, Number: n}
			if !seen[key] {
				issues = append(issues, key)
				seen[key] = true
			}
		}
	}
	return issues
}

// parentFamilyProgress counts the completed, closed, unknown and stopped children of one split
// parent. issueStates caches each project's issue states read for children without jobs.
func (s *Service) parentFamilyProgress(ctx context.Context, p Record, byID map[string]Record, byIssue map[string]FamilyJob, issueStates map[string]map[string]string) Record {
	total := len(fleet.Items(p["splitIssues"]))
	if n := len(fleet.Items(p["splitChildren"])); n > total {
		total = n
	}
	completed, closed, unknown := 0, 0, 0
	stopped := []any{}
	ids := fleet.Items(p["splitChildren"])
	for i := 0; i < total; i++ {
		if i >= len(ids) {
			unknown++
			continue
		}
		c := byID[text(ids[i])]
		if c == nil {
			unknown++
			continue
		}
		issue := record(c["issue"])
		project := text(c["project"])
		if text(issue["project"]) != "" {
			project = text(issue["project"])
		}
		j, exists := byIssue[familyKey(project, issue["number"])]
		status := text(c["status"])
		if exists && j.Status == "merged" && len(j.PendingChecks) > 0 {
			stopped = append(stopped, Record{"project": project, "number": issue["number"], "desk": j.Desk, "status": "verification_waiting", "reason": strings.Join(j.PendingChecks, "、")})
			continue
		}
		if exists && j.Status == "merged" {
			completed++
			continue
		}
		if status == "closed" || status == "submitted" && c["finishedWithoutIssue"] == true || exists && j.Status == "closed" {
			closed++
			continue
		}
		if !exists {
			if issue["number"] != nil && project != "" {
				key := strings.ToLower(project)
				states, loaded := issueStates[key]
				if !loaded {
					if s.options.FamilyIssues != nil {
						states, _ = s.options.FamilyIssues(ctx, project)
					}
					issueStates[key] = states
				}
				switch states[familyKey(project, issue["number"])] {
				case "closed":
					closed++
					continue
				case "open":
					continue
				}
				unknown++
			} else if status == "submitted" || status == "" {
				unknown++
			}
			continue
		}
		reason := j.Reason
		state := j.Status
		if len(j.Waiting) > 0 && state != "failed" && state != "held" && state != "stopped" {
			state = "dependency_waiting"
			reason = strings.Join(j.Waiting, ", ")
		}
		if state == "failed" || state == "held" || state == "stopped" || state == "dependency_waiting" {
			if reason == "" {
				reason = "理由の記録なし"
			}
			stopped = append(stopped, Record{"project": project, "number": issue["number"], "desk": j.Desk, "status": state, "reason": reason})
		}
	}
	return Record{"total": total, "completed": completed, "closed": closed, "remaining": total - completed - closed - unknown, "unknown": unknown, "stopped": stopped}
}
