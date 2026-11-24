package instructionacceptance

import (
	"encoding/json"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
	"strings"
)

type Record = map[string]any

func count(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), n >= 0
	case int64:
		return n, n >= 0
	case float64:
		return int64(n), n >= 0 && n == float64(int64(n))
	case json.Number:
		x, e := n.Int64()
		return x, e == nil && x >= 0
	}
	return 0, false
}
func Check(state, body string, job *jobs.Job, attention, settings Record, readable bool) Record {
	blocks := []any{}
	add := func(reason, explanation string) {
		blocks = append(blocks, Record{"reason": reason, "explanation": explanation})
	}
	runs, holds := recordedCounts(job, readable)
	if state == "closed" || job != nil && (job.Status == "merged" || job.Status == "closed") {
		add("closed", "閉じている依頼です")
	}
	if !readable || state != "open" && state != "closed" || job != nil && (runs == nil || holds == nil) {
		add("unknown", "判定に必要な記録を確かめられません")
	}
	stop := stopReason(attention)
	if attention["implementationRunLimit"] == true {
		kind := "run_limit"
		if strings.HasPrefix(stop, "累計使用量") {
			kind = "usage_limit"
		}
		if stop == "" {
			stop = "実装の回数の上限で停止しています"
		}
		add(kind, stop)
	}
	limit := int64(4)
	if n, ok := count(settings["implementationRunLimit"]); ok && n > 0 && n <= 100 {
		limit = n
	}
	if n, ok := count(runs); ok && n >= limit {
		found := false
		for _, block := range blocks {
			found = found || block.(Record)["reason"] == "run_limit"
		}
		if !found {
			add("run_limit", "実装の回数の上限に達しています")
		}
	}
	reviewLimit := int64(2)
	if n, ok := count(settings["reviewRoundLimit"]); ok && n > 0 && n <= 100 {
		reviewLimit = n
	}
	if n, ok := count(holds); ok && job != nil && job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "review_held" && n >= reviewLimit {
		add("review_round_limit", "レビューの保留の回数の上限に達しています")
	}
	if len(blocks) == 0 {
		return Record{"acceptsInstructions": true}
	}
	var scopePaths any
	if scope := filing.ParseScope(body); scope.State == "declared" {
		scopePaths = len(scope.Paths)
	}
	return Record{"acceptsInstructions": false, "instructionBlocks": blocks, "instructionSize": Record{"scopePaths": scopePaths, "implementationRuns": runs, "reviewHolds": holds, "stopReason": stop}}
}

// recordedCounts reads the implementation runs and review holds of the job;
// nil means the count cannot be confirmed.
func recordedCounts(job *jobs.Job, readable bool) (runs, holds any) {
	if readable {
		runs = int64(0)
		if job != nil {
			runs = jobs.ImplementationRuns(job)
		}
		if job != nil && job.View["reviewHoldsUnknown"] != true {
			if n, ok := count(job.ReviewHolds); ok {
				holds = n
			}
		}
	}
	return runs, holds
}

// stopReason is the recorded stop reason with credentials masked, in at most 300 characters.
func stopReason(attention Record) string {
	stop, _ := attention["reason"].(string)
	stop = consultations.MaskCredentials(strings.Join(strings.Fields(stop), " "))
	if len([]rune(stop)) > 300 {
		stop = string([]rune(stop)[:300])
	}
	return stop
}
