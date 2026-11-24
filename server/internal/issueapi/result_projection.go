package issueapi

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/testcause"
	"issue-desk-intake/server/internal/usage"
)

const (
	reviewFindingsMaxItems  = 50
	reviewFindingsMaxLength = 4000
)

func stringList(value any) []any {
	out := []any{}
	for _, value := range fleet.Items(value) {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// redactedStrings は配列・オブジェクトの入れ子までたどり、文字列の資格情報を伏せる。
func redactedStrings(value any) any {
	switch v := value.(type) {
	case string:
		return usage.Redact(v)
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = usage.Redact(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactedStrings(item)
		}
		return out
	case []Object:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactedStrings(item)
		}
		return out
	case Object:
		out := make(Object, len(v))
		for key, item := range v {
			out[key] = redactedStrings(item)
		}
		return out
	}
	return value
}

// reviewFindingsProjection は保留の判断に要るレビューの本文を依頼者へ返す。
// 今回のレビューが無い回（実装AIの質問で止まった回など）は、Workerが残した前回のレビューを返す。
// uncertainties はWorkerが userJudgments・resolvableUncertainties へ分ける前の過去の結果のために残す。
// 本文は1件最大4000文字×50件×欄の数まで大きくなるので、詳細だけで返し、一覧・検索・画面の同期の行には載せない。
func reviewFindingsProjection(value Object) Object {
	raw, previous := value["review"], false
	if _, ok := raw.(map[string]any); !ok {
		raw, previous = value["previousReview"], true
	}
	review, ok := raw.(map[string]any)
	if !ok || review["omitted"] == true {
		return nil
	}
	list := func(key string) []any {
		out := []any{}
		for _, item := range stringList(review[key]) {
			if len(out) == reviewFindingsMaxItems {
				break
			}
			out = append(out, clipped(usage.Redact(fleet.Text(item)), reviewFindingsMaxLength))
		}
		return out
	}
	out := Object{"previous": previous, "decision": clipped(usage.Redact(fleet.Text(review["decision"])), 20), "summary": clipped(usage.Redact(fleet.Text(review["summary"])), reviewFindingsMaxLength), "breaking": review["breaking"] == true, "tooLarge": review["tooLarge"] == true}
	for _, key := range []string{"findings", "findingKinds", "findingSeverities", "uncertainties", "userJudgments", "resolvableUncertainties", "followups", "improvements", "postMergeChecks"} {
		out[key] = list(key)
	}
	return out
}

// resultProjection は一覧・検索・詳細が共通で返す結果の要約。返す文字列は入れ子まで資格情報を伏せる。
func resultProjection(value Object) Object {
	if value == nil {
		return nil
	}
	status, statusOK := value["status"].(string)
	cause, causeOK := value["fixedTestCause"].(map[string]any)
	if !statusOK && !causeOK {
		return nil
	}
	if !statusOK {
		status = "failed"
	}
	result := Object{"status": status, "summary": fleet.Text(value["summary"]), "questions": stringList(value["questions"]), "unverified": stringList(value["unverified"])}
	if raw, has := value["incomplete"]; has {
		result["incomplete"] = raw
	}
	if raw := value["prerequisiteMissing"]; raw != nil && raw != false && raw != "" && raw != float64(0) {
		result["prerequisiteMissing"] = raw
	}
	if causeOK {
		clean := testcause.Report(cause, 24*1024)
		clean["acquisition"] = clipped(usage.Redact(fleet.Text(cause["acquisition"])), 500)
		result["fixedTestCause"] = clean
	}
	return redactedStrings(result).(Object)
}

// Detailed diagnostics stay out of list rows and their frequent synchronization.
func fixedTestDiagnosticProjection(value Object) Object {
	diagnostic, ok := value["fixedTestDiagnostic"].(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(diagnostic)
	if err != nil || len(raw) > 32*1024 {
		return Object{"unavailable": "検証診断が上限を超えるか読めません"}
	}
	out := Object{}
	for _, key := range []string{"version", "source", "head", "base", "command", "environment", "startedAt", "finishedAt", "timeoutSeconds", "reason", "code", "processStarted", "termination", "userMillis", "systemMillis", "stages", "excerpt"} {
		if v, ok := diagnostic[key]; ok {
			out[key] = v
		}
	}
	return redactedStrings(out).(Object)
}
