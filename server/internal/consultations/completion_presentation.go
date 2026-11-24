package consultations

import "issue-desk-intake/server/internal/completionplan"

// SaveCompletionPresentation stores a detached, validated controller presentation beside the reply.
// The engine and proxy writer append this to saved history; result.messages and
// claimed historical snapshots supplied by the AI are never copied into it.
func SaveCompletionPresentation(message, result Record) {
	p, err := completionplan.Parse(result["completionPlan"])
	if err == nil && text(result["completionProposal"]) == p.Fingerprint() {
		message["completionProposal"] = p.Fingerprint()
		message["completionPlan"] = p.Record()
	}
}
