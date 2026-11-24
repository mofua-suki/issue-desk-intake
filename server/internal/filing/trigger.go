package filing

import (
	"context"
	"issue-desk-intake/server/internal/auth"
)

type checkTriggerKey struct{}

// CheckTrigger returns the trigger associated with the submission context.
func CheckTrigger(ctx context.Context) string {
	value, _ := ctx.Value(checkTriggerKey{}).(string)
	return value
}

// SubmitExplicit preserves Submit's authorization and marks only the interactive entry.
func (s *Service) SubmitExplicit(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	return s.Submit(context.WithValue(ctx, checkTriggerKey{}, "explicit_resend"), id, data, p)
}
