package issueapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

func summarizeDetail(detail Object) Object {
	out := Object{"view": "summary"}
	for _, key := range strings.Fields("executionProgress executionWait executionMissingCapabilities executionReportError executionActive executionScheduled dispatchSkip requiresUser nextActor nextAction finalApprovalTransfers pendingFinalApprovals finalApprovalsUnavailable project number desk intakeId host title shownTitle origin state status workType workTypeLabel followup investigation closedAt closeReason dependencyReason scopeWait projectWait pr error transferPending priority priorityRequest passedOver dispatchRecord deletionApproval permissionApproval overLimitApprovals url actions closeRefusal") {
		if value, has := detail[key]; has {
			out[key] = value
		}
	}
	if progress, ok := detail["executionProgress"].(map[string]any); ok {
		out["executionProgress"] = executionPlanSummary(progress)
	}
	body := fleet.Text(detail["shownBody"])
	if _, has := detail["shownBody"]; !has {
		body = fleet.Text(detail["body"])
	}
	excerpt := []rune(strings.TrimSpace(regexp.MustCompile(`<!-- issue-desk:[\s\S]*?-->`).ReplaceAllString(body, "")))
	out["bodyTruncated"] = len(excerpt) > 200
	if len(excerpt) > 200 {
		excerpt = excerpt[:200]
	}
	out["bodyExcerpt"] = string(excerpt)
	comments := []any{}
	var latest Object
	result := false
	for _, raw := range fleet.Items(detail["comments"]) {
		c := fleet.ObjectOrEmpty(raw)
		if c["kind"] != "log" {
			comments = append(comments, c)
		}
		if c["kind"] == "ask" {
			latest = c
		}
		result = result || c["kind"] == "result"
	}
	out["commentCount"], out["hasResult"], out["latestAsk"] = len(comments), result, nil
	if latest != nil {
		images := latest["images"]
		if images == nil {
			images = []any{}
		}
		out["latestAsk"] = Object{"id": latest["id"], "body": latest["body"], "createdAt": latest["createdAt"], "images": images}
	}
	images := detail["images"]
	if images == nil {
		images = []any{}
	}
	raw, _ := json.Marshal([]any{body, images, comments})
	sum := sha256.Sum256(raw)
	out["contentRevision"] = hex.EncodeToString(sum[:])[:16]
	return out
}
