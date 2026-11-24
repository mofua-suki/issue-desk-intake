package jobs

import (
	"regexp"
	"time"
)

var finalApprovalDiffPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// finalApprovalDiffBinding copies only a complete binding for the current target.
// Old receipts never acquire a fingerprint merely by being read or reported.
func finalApprovalDiffBinding(job *Job) map[string]any {
	binding, _ := job.View["diffFingerprint"].(map[string]any)
	value, _ := binding["value"].(string)
	if _, ok := count(job.Generation); !ok {
		return nil
	}
	if !finalApprovalDiffPattern.MatchString(value) || job.HeadSHA == nil || !shaPattern.MatchString(*job.HeadSHA) || job.PR == nil || *job.PR < 1 || job.Branch == nil || *job.Branch == "" {
		return nil
	}
	target := map[string]any{"jobId": job.ID, "generation": job.Generation, "pr": *job.PR, "branch": *job.Branch, "headSha": *job.HeadSHA, "value": value}
	for key, expected := range target {
		if !equivalent(binding[key], expected) {
			return nil
		}
	}
	return target
}

func finalApprovalDiffMatches(job *Job, record map[string]any) bool {
	current := finalApprovalDiffBinding(job)
	original, _ := record["diffFingerprint"].(map[string]any)
	head, _ := record["headSha"].(string)
	if current == nil || original == nil || !shaPattern.MatchString(head) || original["headSha"] != head {
		return false
	}
	oldGeneration, valid := count(original["generation"])
	if !valid || job.Generation < oldGeneration {
		return false
	}
	for _, key := range []string{"jobId", "pr", "branch", "value"} {
		if !equivalent(original[key], current[key]) {
			return false
		}
	}
	return true
}

// finalApprovalTransferred requires the audit receipt committed by Report,
// as well as the current binding. History alone cannot authorize a transfer.
func finalApprovalTransferred(job *Job, record map[string]any) bool {
	if !finalApprovalDiffMatches(job, record) {
		return false
	}
	history, _ := job.View["finalApprovalTransfers"].([]any)
	for _, item := range history {
		entry, ok := item.(map[string]any)
		if !ok || entry["id"] != record["id"] || entry["fingerprint"] != record["fingerprint"] || entry["messageId"] != record["messageId"] || entry["fromHead"] != record["headSha"] || entry["toHead"] != stringValue(job.HeadSHA) || entry["diffFingerprint"] != record["diffFingerprint"].(map[string]any)["value"] {
			continue
		}
		original := record["diffFingerprint"].(map[string]any)
		from, fromOK := count(original["generation"])
		entryFrom, hasFrom := entry["fromGeneration"]
		entryTo, hasTo := entry["toGeneration"]
		if !hasFrom && !hasTo {
			if !fromOK || from != job.Generation {
				continue
			}
		} else if !hasFrom || !hasTo || !equivalent(entryFrom, from) || !equivalent(entryTo, job.Generation) {
			continue
		}
		at, _ := entry["at"].(string)
		if _, err := time.Parse(time.RFC3339Nano, at); err == nil {
			return true
		}
	}
	return false
}

// recordFinalApprovalTransfers runs after the state update, inside its transaction.
// PendingFinalApprovals remains a read-only calculation, including after reload.
func recordFinalApprovalTransfers(job *Job, at time.Time) bool {
	// Obtain the current requirements without treating existing receipts as fulfilled.
	copyJob := *job
	copyJob.View = make(map[string]any, len(job.View))
	for key, value := range job.View {
		copyJob.View[key] = value
	}
	delete(copyJob.View, "finalApprovals")
	required, err := PendingFinalApprovals(&copyJob)
	if err != nil {
		return false
	}
	records, _ := job.View["finalApprovals"].([]any)
	history, _ := job.View["finalApprovalTransfers"].([]any)
	changed := false
	for _, approval := range required {
		for _, item := range records {
			record, ok := item.(map[string]any)
			if !ok || record["id"] != approval.ID || record["fingerprint"] != approval.Fingerprint || record["headSha"] == approval.HeadSHA || !finalApprovalDiffMatches(job, record) {
				continue
			}
			binding := record["diffFingerprint"].(map[string]any)
			fromGeneration, _ := count(binding["generation"])
			transfer := map[string]any{"fromGeneration": fromGeneration, "toGeneration": job.Generation, "id": approval.ID, "fingerprint": approval.Fingerprint, "messageId": record["messageId"], "fromHead": record["headSha"], "toHead": approval.HeadSHA, "diffFingerprint": binding["value"]}
			found := false
			for _, previous := range history {
				entry, ok := previous.(map[string]any)
				if !ok {
					continue
				}
				matches := true
				for key, value := range transfer {
					// Old same-generation receipts have no generation fields.
					if (key == "fromGeneration" || key == "toGeneration") && fromGeneration == job.Generation && entry["fromGeneration"] == nil && entry["toGeneration"] == nil {
						continue
					}
					if !equivalent(entry[key], value) {
						matches = false
						break
					}
				}
				if matches {
					found = true
					break
				}
			}
			if !found {
				transfer["at"] = at.UTC().Format(time.RFC3339Nano)
				history = append(history, transfer)
				changed = true
			}
		}
	}
	if changed {
		job.View["finalApprovalTransfers"] = history
	}
	return changed
}
