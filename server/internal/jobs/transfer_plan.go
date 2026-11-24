package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"time"
)

type transferJob struct {
	ID            int64
	Repo          string
	Issue         int64
	Title, Status string
	Provider      string
	View, Result  map[string]any
	Deps          []int64
}
type transferPlan struct {
	Blocking      bool
	Checks, Waits []any
	MissingSince  map[string]any
	OrphanedWaits []any
	Targets       []int64
	TargetChecks  map[int64][]string
	TargetPaths   map[int64][][]string
}

func transferKey(repo string, issue int64) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repo), issue)
}
func transferJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }
func transferContext(job, source *transferJob, transfer map[string]any) (string, map[string]any) {
	records := fleet.Object(source.View["acceptanceTransferRecords"])
	proposals := fleet.List(records["proposals"])
	numbers := fleet.List(transfer["followupNumbers"])
	includes := func(value any) bool {
		for _, n := range numbers {
			if n == value {
				return true
			}
		}
		return false
	}
	produces, all := []any{}, []any{}
	for _, raw := range proposals {
		p := fleet.Object(raw)
		if !includes(p["number"]) {
			continue
		}
		all = append(all, map[string]any{"id": p["id"], "number": p["number"]})
		issue := fleet.Object(p["issue"])
		if p["status"] == "filed" && transferKey(fleet.Text(issue["project"]), int64(fleet.Number(issue["number"]))) == transferKey(job.Repo, job.Issue) {
			produces = append(produces, p["id"])
		}
	}
	instructions := job.View["instructions"]
	if instructions == nil {
		instructions = []any{}
	}
	context := map[string]any{"request": map[string]any{"repo": job.Repo, "issue": job.Issue, "title": job.Title, "instructions": instructions, "input": job.View["acceptanceTransferInput"]}, "source": transferKey(source.Repo, source.Issue), "criterion": transfer["criterion"], "artifacts": transfer["artifacts"], "produces": produces, "proposals": all}
	// Existing evidence is retained only for an identical complete request context.
	for _, raw := range fleet.List(fleet.Object(job.View["acceptanceTransferWait"])["checks"]) {
		check := fleet.Object(raw)
		if reflect.DeepEqual(check["context"], fleet.Clone(context)) {
			return fleet.Text(check["key"]), context
		}
	}
	request := fleet.Object(context["request"])
	encoded := `{"request":{"repo":` + transferJSON(request["repo"]) + `,"issue":` + transferJSON(request["issue"]) + `,"title":` + transferJSON(request["title"]) + `,"instructions":` + transferJSON(request["instructions"]) + `,"input":` + transferJSON(request["input"]) + `},"source":` + transferJSON(context["source"]) + `,"criterion":` + transferJSON(context["criterion"]) + `,"artifacts":` + transferJSON(context["artifacts"]) + `,"produces":` + transferJSON(produces) + `,"proposals":` + transferJSON(all) + `}`
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), context
}

type transferIndex struct {
	byID  map[int64]*transferJob
	byKey map[string]*transferJob
}

func newTransferIndex(all []*transferJob) *transferIndex {
	index := &transferIndex{byID: map[int64]*transferJob{}, byKey: map[string]*transferJob{}}
	for _, j := range all {
		index.byID[j.ID] = j
		index.byKey[transferKey(j.Repo, j.Issue)] = j
	}
	return index
}
func (index *transferIndex) plan(job *transferJob) transferPlan {
	return index.planAt(job, time.Now().UTC())
}

const transferMissingGrace = time.Hour

func (index *transferIndex) planAt(job *transferJob, now time.Time) transferPlan {
	p := transferPlan{MissingSince: map[string]any{}, OrphanedWaits: []any{}, Checks: []any{}, Waits: []any{}, Targets: []int64{}, TargetChecks: map[int64][]string{}, TargetPaths: map[int64][][]string{}}
	byID, byKey := index.byID, index.byKey
	dependencies := func(j *transferJob) []int64 {
		out := []int64{}
		owned := fleet.Object(fleet.Object(j.View["acceptanceTransferWait"])["ownedTargets"])
		seen := map[int64]bool{}
		for _, id := range j.Deps {
			if owned[fmt.Sprint(id)] == nil && !seen[id] {
				out = append(out, id)
				seen[id] = true
			}
		}
		for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
			for _, raw := range fleet.List(j.View[field]) {
				d := fleet.Object(raw)
				target := byKey[transferKey(fleet.Text(d["repo"]), int64(fleet.Number(d["issue"])))]
				if target != nil && !seen[target.ID] {
					out = append(out, target.ID)
					seen[target.ID] = true
				}
			}
		}
		return out
	}
	checks := map[string]bool{}
	targets := map[int64]bool{}
	var visit func(*transferJob, []string, map[int64]bool)
	visit = func(source *transferJob, path []string, ancestors map[int64]bool) {
		if source == nil || source.ID == job.ID || ancestors[source.ID] {
			return
		}
		branch := map[int64]bool{}
		for id, v := range ancestors {
			branch[id] = v
		}
		branch[source.ID] = true
		records := fleet.Object(source.View["acceptanceTransferRecords"])
		report := fleet.Object(records["report"])
		for _, raw := range fleet.List(report["acceptanceTransfers"]) {
			transfer := fleet.Object(raw)
			numbers := fleet.List(transfer["followupNumbers"])
			proposals := []map[string]any{}
			for _, raw := range fleet.List(records["proposals"]) {
				proposal := fleet.Object(raw)
				included := false
				for _, number := range numbers {
					if number == proposal["number"] {
						included = true
					}
				}
				issue := fleet.Object(proposal["issue"])
				if included && !(proposal["status"] == "filed" && transferKey(fleet.Text(issue["project"]), int64(fleet.Number(issue["number"]))) == transferKey(job.Repo, job.Issue)) {
					proposals = append(proposals, proposal)
				}
			}
			if len(proposals) == 0 {
				continue
			}
			key, context := transferContext(job, source, transfer)
			saved := fleet.Object(fleet.Object(job.View["acceptanceTransferChecks"])[key])
			input := fleet.Object(job.View["acceptanceTransferInput"])
			relation := "unknown"
			if input["unavailable"] != true && (saved["relation"] == "unrelated" || saved["relation"] == "required") {
				relation = fleet.Text(saved["relation"])
			}
			evidence := saved["evidence"]
			if evidence == nil {
				evidence = "利用関係が未確認です"
			}
			if input["unavailable"] == true {
				evidence = "現在のIssue本文を取得できません。次の周期または配布前に再取得します。"
			}
			first := !checks[key]
			if first {
				checks[key] = true
				p.Checks = append(p.Checks, map[string]any{"key": key, "context": context, "relation": relation, "evidence": evidence, "consultationId": saved["consultationId"]})
			}
			if relation == "unrelated" {
				continue
			}
			for _, proposal := range proposals {
				issue := fleet.Object(proposal["issue"])
				destination := byKey[transferKey(fleet.Text(issue["project"]), int64(fleet.Number(issue["number"])))]
				if relation == "required" && proposal["status"] == "filed" && destination != nil {
					id := destination.ID
					if !targets[id] {
						p.Targets = append(p.Targets, id)
						targets[id] = true
					}
					p.TargetChecks[id] = appendUniqueString(p.TargetChecks[id], key)
					route := append(append([]string{}, path...), key)
					p.TargetPaths[id] = append(p.TargetPaths[id], route)
					visit(destination, route, branch)
				}
				if first && (relation == "unknown" || proposal["status"] != "filed" || destination == nil || destination.Status != "merged" || !shaPattern.MatchString(fleet.Text(destination.Result["mergeCommit"]))) {
					reason := evidence
					if relation != "unknown" {
						switch {
						case proposal["status"] != "filed":
							reason = proposal["reason"]
						case destination == nil:
							reason = "起票先ジョブの取り込み待ち"
						default:
							reason = "移管先PRのマージ待ち（未マージのクローズでは解除しません）"
						}
					}
					wait := map[string]any{"source": records["source"], "criterion": transfer["criterion"], "artifacts": transfer["artifacts"], "proposalId": proposal["id"], "destination": proposal["issue"], "reason": reason, "consultationId": saved["consultationId"]}
					id := fleet.Text(proposal["id"])
					if relation == "required" && proposal["status"] == "missing" && id != "" {
						previous := fleet.Object(fleet.Object(job.View["acceptanceTransferWait"])["missingSince"])
						since, err := time.Parse(time.RFC3339Nano, fleet.Text(previous[id]))
						if err != nil || since.After(now) {
							since = now
						}
						stamp := since.UTC().Format(time.RFC3339Nano)
						p.MissingSince[id] = stamp
						if now.Sub(since) >= transferMissingGrace {
							wait["reason"] = "受付に案の記録が無く起票されないため待たない"
							wait["missingSince"] = stamp
							p.OrphanedWaits = append(p.OrphanedWaits, wait)
							continue
						}
					}
					p.Waits = append(p.Waits, wait)
				}
			}
		}
		for _, id := range dependencies(source) {
			visit(byID[id], path, branch)
		}
	}
	for _, id := range dependencies(job) {
		visit(byID[id], nil, map[int64]bool{})
	}
	p.Blocking = len(p.Waits) > 0
	return p
}
func appendUniqueString(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}
