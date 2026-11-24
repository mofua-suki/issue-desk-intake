package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"slices"
)

// Only the statistics can derive lease expiry from a matching event.
var endReasons = append(slices.Clone(jobs.ImplementationEndReasons), "lease-expired")

// savedImplementationEnd returns the implementation end saved on a terminal
// event when it is valid for the lease the run belongs to, and whether a
// record was saved at all. Both statistics paths use it so that a malformed
// record is never treated as "no record" and never inferred around.
//
// The intake generation (the record's generation, compared with the lease's)
// is validated separately from the usage row's local generation. Generation 0
// is valid. Only a missing record generation is filled, and only from the
// lease, which the callers match uniquely by job, host and epoch; null,
// malformed or mismatching values make the record invalid.
func savedImplementationEnd(eventData, leaseData map[string]any) (AccountRecord, bool) {
	raw, present := eventData["implementationEnd"]
	if !present {
		return nil, false
	}
	r, ok := raw.(map[string]any)
	if !ok {
		return nil, true
	}
	leaseGeneration, ok := leaseData["generation"]
	if !ok || !accountWhole(leaseGeneration) {
		return nil, true
	}
	generation, ok := r["generation"]
	if !ok {
		generation = leaseGeneration
	}
	unknown, ok := r["usageUnknown"].(bool)
	if fleet.Number(r["version"]) != 1 || !accountWhole(generation) || generation != leaseGeneration || !includes(jobs.ImplementationEndReasons, fleet.Text(r["reason"])) || !ok || unknown && r["reason"] != "guard" {
		return nil, true
	}
	out := AccountRecord{}
	for key, value := range r {
		out[key] = value
	}
	out["generation"] = generation
	return out, true
}

// implementationEndRecord is savedImplementationEnd for the rework path.
func implementationEndRecord(event, lease AccountRecord) (AccountRecord, bool) {
	return savedImplementationEnd(fleet.Object(event["data"]), fleet.Object(lease["data"]))
}

// classifyImplementation prefers a valid saved reason other than unknown. A
// malformed record stays unknown. Only an unknown or missing record is
// inferred, within the rework path's safety conditions (the snapshot is the
// job row only when it matches the terminal event).
func classifyImplementation(rows []AccountRecord, event, snapshot, record AccountRecord, recorded, progressed bool) string {
	if record != nil && record["reason"] != "unknown" {
		return fleet.Text(record["reason"])
	}
	if recorded && record == nil {
		return "unknown"
	}
	data := fleet.Object(event["data"])
	switch fleet.Text(data["holdReason"]) {
	case "question":
		return "question"
	case "prerequisite_missing":
		return "prerequisite"
	case "review_held":
		return "review-hold"
	}
	if data["to"] == "stopped" {
		return "stopped"
	}
	// A merge ends the run by its own state; the snapshot may still hold an
	// earlier failure because the job result is kept across runs.
	if data["to"] == "merged" {
		if len(rows) > 0 && rows[len(rows)-1]["outcome"] == "completed" {
			return "success"
		}
		return "unknown"
	}
	// A saved unknown already classified the reported result; like /api/stats,
	// only the hold reason and state above may refine it.
	if recorded {
		return "unknown"
	}
	reason, usageUnknown := jobs.ClassifyImplementationFailure(fleet.Object(snapshot["result"]), fleet.Object(snapshot["view"]))
	if usageUnknown || reason == "usage-limit" {
		return reason
	}
	for _, row := range rows {
		if row["outcome"] == "timeout" {
			return "timeout"
		}
	}
	if reason != "unknown" {
		return reason
	}
	// A requeued lease may expire after implementation has already completed.
	// Only evidence from a review under that same lease can prove progression.
	leaseExpired := event["kind"] == "lease_expired"
	requeued := leaseExpired && data["to"] == "queued"
	if (event == nil || requeued) && progressed && len(rows) > 0 && rows[len(rows)-1]["outcome"] == "completed" {
		return "success"
	}
	// The matching expiry is an observed end even when no completion was reported.
	if requeued || leaseExpired && data["to"] == "failed" {
		return "lease-expired"
	}
	return "unknown"
}
