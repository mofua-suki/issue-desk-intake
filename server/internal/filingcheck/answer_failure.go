package filingcheck

import (
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"

	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/usage"
)

var diagnosticID = regexp.MustCompile(`^[a-f0-9]{16}$`)
var diagnosticModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`)

// Protocol fields use allowlists; model identifiers are validated by format.
var answerFields = map[string]map[string]bool{
	"kind":            failureAllowed("completed", "timeout_idle", "timeout_total", "canceled_by_server", "spawn_failed", "output_overflow", "exit_nonzero", "authentication", "rate_limit", "usage_limit", "format", "session", "input_invalid", "execution"),
	"stage":           failureAllowed("preflight", "spawn", "run", "exit", "answer", "done"),
	"provider":        failureAllowed("codex", "claude"),
	"effort":          failureAllowed("minimal", "low", "medium", "high", "xhigh", "max"),
	"step":            failureAllowed(usage.FilingCheckStep),
	"finalEvent":      failureAllowed("turn.started", "turn.completed", "turn.failed", "thread.started", "item.started", "item.completed", "item.updated", "error", "system", "assistant", "result", "stream_event"),
	"originalFailure": failureAllowed("error", "authentication", "rate_limit", "usage_limit", "format", "session", "input_size", "schema", "timeout", "stalled", "execution", "usage_unmeasured"),
	"reason":          failureAllowed("record_failed", "missing", "partial"),
}

func failureAllowed(values ...string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

func safeDiagnostic(input Record) Record {
	out := Record{}
	for _, key := range []string{"kind", "stage", "provider", "model", "effort", "step", "finalEvent"} {
		v := fleet.Text(input[key])
		if (key == "model" && diagnosticModel.MatchString(v)) || answerFields[key][v] {
			out[key] = v
		}
	}
	if id := fleet.Text(input["id"]); diagnosticID.MatchString(id) {
		out["id"] = id
	}
	for _, key := range []string{"elapsedMs", "firstOutputMs", "lastOutputMs", "stdoutBytes", "stderrBytes", "exitCode", "attempt", "turn"} {
		v, exists := input[key]
		if !exists || v == nil {
			continue
		}
		n := fleet.WholeNumber(v)
		if n >= 0 && n <= 2147483647 && fmt.Sprint(v) == fmt.Sprint(n) {
			out[key] = n
		}
	}
	return out
}

func answerFailure(d Record, err error) Record {
	var failure *consultationai.AnswerFailure
	if !errors.As(err, &failure) {
		return nil
	}
	result := Record{}
	entries := []any{}
	for _, item := range failure.Diagnostics {
		if row := safeDiagnostic(obj(item)); len(row) > 0 {
			entries = append(entries, row)
		}
	}
	hold := obj(failure.MeasurementHold)
	// A completed call may have a hold but no failed diagnostic history entry.
	if diagnostic := obj(hold["diagnostic"]); diagnostic != nil {
		id := fleet.Text(diagnostic["id"])
		found := false
		for _, item := range entries {
			if id != "" && fleet.Text(obj(item)["id"]) == id {
				found = true
				break
			}
		}
		if !found {
			for _, key := range []string{"provider", "model", "step"} {
				diagnostic[key] = hold[key]
			}
			if row := safeDiagnostic(diagnostic); len(row) > 0 {
				entries = append(entries, row)
			}
		}
	}
	if len(entries) > 0 {
		result["diagnostics"] = entries
	}
	for _, key := range []string{"reason", "originalFailure"} {
		if v := fleet.Text(hold[key]); answerFields[key][v] {
			result[key] = v
		}
	}
	if len(result) == 0 {
		return nil
	}
	cause := "AI呼び出しの診断を記録しました。"
	switch fleet.Text(result["reason"]) {
	case "missing":
		cause += "使用量が返りませんでした。"
	case "partial":
		cause += "使用量の一部が欠けていました。"
	case "record_failed":
		cause += "使用量の記録を保存できませんでした。"
	}
	result["cause"] = consultations.FailureCause(d, errors.New(cause))
	return result
}
