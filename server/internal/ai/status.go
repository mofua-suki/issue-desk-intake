package ai

import "context"

// Availability is safe to expose; provider output and credential paths never enter it.
type Availability struct {
	Provider string `json:"provider"`
	Ready    bool   `json:"ready"`
	Reason   string `json:"reason"`
}

func (r *Runner) Status(ctx context.Context, provider, cwd string) Availability {
	status := Availability{Provider: provider, Reason: "unavailable"}
	run := r.Execute
	if run == nil {
		run = Run
	}
	var err error
	switch provider {
	case "codex":
		exe := r.Codex
		if exe == "" {
			exe = "codex"
		}
		err = r.codexReady(ctx, run, exe, cwd)
	case "claude":
		exe := r.Claude
		if exe == "" {
			exe = "claude"
		}
		err = r.claudeReady(ctx, run, exe, cwd)
	default:
		return status
	}
	if err != nil {
		status.Reason = "authentication"
		return status
	}
	status.Ready, status.Reason = true, "ready"
	return status
}
