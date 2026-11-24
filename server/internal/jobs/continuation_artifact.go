package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

// artifactIdentity is kept inside the intake-owned continuation, never accepted from a worker.
type artifactIdentity struct {
	Generation          int64
	Branch, Head, Owner string
}

func artifactOf(job *Job) (artifactIdentity, bool) {
	c := fleet.Object(job.View["continuation"])
	a := fleet.Object(c["artifact"])
	generation, ok := count(a["generation"])
	branch, branchOK := a["branch"].(string)
	head, _ := a["headSha"].(string)
	owner, _ := a["owner"].(string)
	if !ok || generation > job.Generation || !branchOK || head == "" || owner == "" || job.HeadSHA == nil || *job.HeadSHA != head || stringValue(job.Branch) != branch {
		return artifactIdentity{}, false
	}
	return artifactIdentity{generation, branch, head, owner}, true
}

// Only absence is legacy. A present null, malformed continuation or inconsistent
// artifact must not become an opportunity to infer ownership from lease history.
func legacyArtifactMissing(job *Job) bool {
	raw, exists := job.View["continuation"]
	if !exists {
		return true
	}
	c, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	_, exists = c["artifact"]
	return !exists
}

// legacyArtifactOwner must run after fencing under the job row lock. Dispatch
// takes the same lock and records each lease in its transaction. Read the entire
// intake history, not Detail's recent-event window or a worker-supplied marker.
func legacyArtifactOwner(ctx context.Context, tx *sql.Tx, job *Job, host string) (bool, error) {
	if job.HeadSHA == nil || !legacyArtifactMissing(job) || job.LeaseEpoch < 1 || stringValue(job.LeaseHost) != host {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT host,data FROM job_events WHERE job_id=$1 AND kind='leased' ORDER BY seq`, job.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	expected := int64(1)
	for rows.Next() {
		var owner sql.NullString
		var raw []byte
		if err := rows.Scan(&owner, &raw); err != nil {
			return false, err
		}
		var data map[string]any
		if json.Unmarshal(raw, &data) != nil {
			return false, nil
		}
		epoch, ok := count(data["epoch"])
		if !owner.Valid || owner.String != host || !ok || epoch != expected || epoch > job.LeaseEpoch {
			return false, nil
		}
		expected++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return expected == job.LeaseEpoch+1, nil
}
func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// artifactAvailable is intentionally conservative until publication evidence is recorded by intake.
func artifactAvailable(a artifactIdentity, host string, published bool) bool {
	return a.Head != "" && (a.Owner != "" && a.Owner == host || published)
}

// artifactDispatchable mirrors the Report guard in updateReportArtifact. A host that
// neither owns nor can verify the publication of the current artifact is refused
// every head change, so leasing the job to it only lets the lease expire.
// Legacy and reset jobs keep the Report-side decision.
func artifactDispatchable(job *Job, host string) bool {
	if job.HeadSHA == nil || artifactFromBase(job) {
		return true
	}
	a, known := artifactOf(job)
	if !known {
		return true
	}
	return artifactAvailable(a, host, artifactPublished(job, a))
}

func artifactPublished(job *Job, a artifactIdentity) bool {
	evidence := fleet.Object(fleet.Object(job.View["continuation"])["publication"])
	generation, ok := count(evidence["generation"])
	if rawPR, exists := evidence["pr"]; exists {
		pr, valid := count(rawPR)
		if !valid || job.PR == nil || pr != *job.PR {
			return false
		}
	}
	return ok && generation == a.Generation && evidence["branch"] == a.Branch && evidence["headSha"] == a.Head && evidence["verified"] == true
}
func artifactFromBase(job *Job) bool {
	reset := fleet.Object(job.View["retryFromBase"])
	generation, ok := count(reset["generation"])
	by, _ := reset["by"].(string)
	return ok && generation == job.Generation && (by == "issue-screen" || by == "cli-key" || by == artifactResetBy || strings.HasPrefix(by, "token:") && len(by) > len("token:"))
}

// artifactResetBy marks a generation that dispatch moved back to the default
// branch because no host could ever report a head change for it.
const artifactResetBy = "dispatch:artifact_unknown"

// artifactUnreportable reports whether Report would refuse every head change from
// host after this lease: the head exists, no reset applies, the recorded artifact
// is unknown (invalid or a null tombstone) and no legacy lease history makes host
// its owner. Leasing such a job only lets the lease expire until it fails, so
// dispatch starts the generation from the default branch instead. It must run
// under the job row lock taken by Next.
func artifactUnreportable(ctx context.Context, tx *sql.Tx, job *Job, host string) (bool, error) {
	if job.HeadSHA == nil || artifactFromBase(job) {
		return false, nil
	}
	if _, known := artifactOf(job); known {
		return false, nil
	}
	if !legacyArtifactMissing(job) {
		return true, nil
	}
	if job.LeaseEpoch < 1 {
		// The first lease is issued to host, so Report sees host as the only owner.
		return false, nil
	}
	leased := *job
	leased.LeaseHost = &host
	owner, err := legacyArtifactOwner(ctx, tx, &leased, host)
	return !owner, err
}

// carryArtifact supplies Control with a validated handoff without inferring an owner
// from the last lease. The caller decides whether the retry preserves the artifact.
func carryArtifact(job *Job, continuation map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range continuation {
		result[key] = value
	}
	delete(result, "artifact")
	delete(result, "publication")
	a, ok := artifactOf(job)
	if !ok {
		if !legacyArtifactMissing(job) {
			// Do not transfer an invalid identity. Keep a null tombstone so a
			// normal retry cannot launder its presence into legacy absence.
			result["artifact"] = nil
		}
		return result
	}
	result["artifact"] = map[string]any{"generation": a.Generation, "branch": a.Branch, "headSha": a.Head, "owner": a.Owner}
	if artifactPublished(job, a) {
		publication := map[string]any{"generation": a.Generation, "branch": a.Branch, "headSha": a.Head, "verified": true}
		if pr, exists := fleet.Object(fleet.Object(job.View["continuation"])["publication"])["pr"]; exists {
			publication["pr"] = pr
		}
		result["publication"] = publication
	}
	return result
}

// updateReportArtifact returns a replacement continuation only when the tuple changes.
func updateReportArtifact(job *Job, host string, report Report, legacyOwner bool) (map[string]any, error) {
	branch, head := job.Branch, job.HeadSHA
	if report.Branch.Present {
		branch = report.Branch.Value
	}
	if report.HeadSHA.Present {
		head = report.HeadSHA.Value
	}
	if same(branch, job.Branch) && same(head, job.HeadSHA) {
		return nil, nil
	}
	old, known := artifactOf(job)
	resetGeneration := artifactFromBase(job) && (!known || old.Generation != job.Generation)
	sameHeadReset := resetGeneration && report.Status == "running" && report.Branch.Present && report.Branch.Value != nil &&
		report.HeadSHA.Present && report.HeadSHA.Value != nil && job.HeadSHA != nil && same(head, job.HeadSHA) &&
		*report.Branch.Value == fmt.Sprintf("codex/issue-%d-job-%d-retry-%d", job.Issue, job.ID, job.Generation)
	reset := resetGeneration && (!same(head, job.HeadSHA) || sameHeadReset)
	legacyOwner = legacyOwner && legacyArtifactMissing(job)
	if job.HeadSHA != nil && !reset && !legacyOwner && (!known || !artifactAvailable(old, host, artifactPublished(job, old))) {
		return nil, contract.NewProblem(409, "元の成果を取得できないため変更できません。", "artifact_unavailable")
	}
	c := map[string]any{}
	for k, v := range fleet.Object(job.View["continuation"]) {
		c[k] = v
	}
	delete(c, "artifact")
	delete(c, "publication")
	if head != nil {
		// Branch-only changes keep ownership; a new head belongs to its reporter.
		owner := host
		if known && same(head, job.HeadSHA) && !sameHeadReset {
			owner = old.Owner
		}
		c["artifact"] = map[string]any{"generation": job.Generation, "branch": stringValue(branch), "headSha": *head, "owner": owner}
	}
	return c, nil
}
