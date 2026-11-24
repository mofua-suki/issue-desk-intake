package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Diagnostic describes one provider call with fixed identifiers and numbers only.
// It never holds a prompt, an answer, stderr text, an environment value, a credential or a URL.
// Kind is one of: completed, timeout_idle, timeout_total, canceled_by_server, spawn_failed, output_overflow,
// exit_nonzero, authentication, rate_limit, usage_limit, format, session, input_invalid, execution.
// Stage is one of: preflight, spawn, run, exit, answer, done.
type Diagnostic struct {
	ID            string
	Kind          string
	Stage         string
	ElapsedMs     int64
	FirstOutputMs *int64
	LastOutputMs  *int64
	StdoutBytes   int
	StderrBytes   int
	ExitCode      *int
	FinalEvent    string
	FailureReason string
	UsageEvidence string
	// Idle is set only for timeout_idle.
	Idle          *IdleSnapshot
	IdleCondition string
}

// Map is the persisted form (JSON-friendly). Unknown values are nil, never 0.
func (d Diagnostic) Map() map[string]any {
	number := func(v *int64) any {
		if v == nil {
			return nil
		}
		return float64(*v)
	}
	var exit any
	if d.ExitCode != nil {
		exit = float64(*d.ExitCode)
	}
	var final any
	if d.FinalEvent != "" {
		final = d.FinalEvent
	}
	out := map[string]any{
		"id": d.ID, "kind": d.Kind, "stage": d.Stage, "elapsedMs": float64(d.ElapsedMs),
		"firstOutputMs": number(d.FirstOutputMs), "lastOutputMs": number(d.LastOutputMs),
		"stdoutBytes": float64(d.StdoutBytes), "stderrBytes": float64(d.StderrBytes),
		"exitCode": exit, "finalEvent": final,
	}
	if d.FailureReason != "" {
		out["failureReason"] = d.FailureReason
	}
	if d.UsageEvidence != "" {
		out["usageEvidence"] = d.UsageEvidence
	}
	if d.Kind == "timeout_idle" && (d.IdleCondition == "output_deadline" || d.IdleCondition == "inactivity_deadline") {
		out["idleCondition"] = d.IdleCondition
	}
	if d.Idle != nil {
		out["idleProcess"] = d.Idle.Map()
	}
	return out
}

// lastEventType returns the type of the last JSON line of provider output, only if it is a short identifier.
func lastEventType(stdout []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	var row map[string]any
	if json.Unmarshal([]byte(lines[len(lines)-1]), &row) == nil {
		name, _ := row["type"].(string)
		switch name {
		case "thread.started", "turn.started", "turn.completed", "turn.failed", "item.started", "item.updated", "item.completed", "error", "system", "assistant", "user", "result", "stream_event":
			return name
		}
	}
	return ""
}

// failureReason exposes only fixed identifiers, never the source text. Only the
// last turn.failed error, Claude error results and stderr are evidence; ordinary answers are not.
func failureReason(process ProcessResult) string {
	var message string
	for _, row := range events(process.Stdout) {
		if row["type"] == "turn.failed" {
			message, _ = object(row["error"])["message"].(string)
		} else if row["type"] == "result" && row["is_error"] == true {
			message, _ = row["result"].(string)
		}
	}
	text := strings.ToLower(message + "\n" + string(process.Stderr))
	for _, rule := range []struct{ pattern, reason string }{
		{`usage.limit|hit.your.limit|weekly.limit|daily.limit|quota.exceeded`, "usage_limit"},
		{`rate.limit|too.many.requests|\b429\b`, "rate_limit"},
		{`unauthorized|unauthenticated|authentication|oauth|not.logged.in|invalid.api.key|\b401\b`, "authentication"},
		{`invalid.request|invalid.input|context.length.exceeded|prompt.is.too.long|input.is.too.long|input.too.long|maximum.context.length|exceeds.{0,30}(context|token|input).{0,20}(limit|length|maximum)|\b400\b`, "input_invalid"},
		{`overloaded|overload_error|service.unavailable|internal.server.error|server.error|\b(?:http(?:/[0-9.]+)?|status(?:[ _-]+code)?|error(?:[ _-]+code)?)[ :=_-]+5[0-9][0-9]\b|\b5[0-9][0-9][ :=_-]+(?:http|status|error)\b|connection.(reset|closed|lost|refused)|connection.error|socket.hang.up`, "provider_unavailable"},
	} {
		if regexp.MustCompile(rule.pattern).MatchString(text) {
			return rule.reason
		}
	}
	return "unknown"
}

func newDiagnosticID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b)
}

func millis(d time.Duration) *int64 {
	if d < 0 {
		return nil
	}
	v := d.Milliseconds()
	return &v
}

func diagnosticKind(process ProcessResult, ran bool, err error) string {
	if err == nil {
		return "completed"
	}
	if ran {
		switch process.Cause {
		case "timeout_idle", "timeout_total", "canceled_by_server", "spawn_failed", "output_overflow", "exit_nonzero":
			return process.Cause
		}
	}
	var failure *Failure
	if errors.As(err, &failure) {
		switch failure.Kind {
		case "authentication", "rate_limit", "usage_limit", "format", "session":
			return failure.Kind
		case "input_size", "schema":
			return "input_invalid"
		case "timeout", "stalled":
			return "timeout_total"
		}
		return "execution"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled_by_server"
	}
	return "execution"
}

func diagnose(stage string, process ProcessResult, ran bool, err error, started time.Time) Diagnostic {
	d := Diagnostic{ID: newDiagnosticID(), Stage: stage, ElapsedMs: time.Since(started).Milliseconds(), Kind: diagnosticKind(process, ran, err)}
	if ran {
		d.FirstOutputMs, d.LastOutputMs = millis(process.FirstOutput), millis(process.LastOutput)
		d.StdoutBytes, d.StderrBytes = process.StdoutBytes, process.StderrBytes
		d.FinalEvent = lastEventType(process.Stdout)
		d.Idle = process.Idle
		if d.Kind == "timeout_idle" {
			d.IdleCondition = process.IdleCondition
		}
		if process.Cause != "spawn_failed" && process.Cause != "canceled_by_server" && process.Cause != "timeout_idle" && process.Cause != "timeout_total" {
			code := process.Code
			d.ExitCode = &code
		}
		if process.Cause == "exit_nonzero" {
			d.FailureReason = failureReason(process)
		}
	}
	return d
}
