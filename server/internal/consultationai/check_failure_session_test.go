package consultationai

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

// 回答の検査で断って終わったターンでも、会話は保存済みの返答で終わっているので残し、
// 次の発言を全文の入れ直しでなく差分で続けられるようにする。
func TestReplyCheckFailureKeepsSession(t *testing.T) {
	err := runExecutionRepairKeep(t)
	var failure *ai.Failure
	if !errors.As(err, &failure) || failure.Kind != "reply_check" {
		t.Fatalf("err=%v", err)
	}
}

func runExecutionRepairKeep(t *testing.T) error {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
		if request.OnSession != nil && request.Session == "" {
			if err := request.OnSession("11111111-1111-4111-8111-111111111113"); err != nil {
				t.Fatal(err)
			}
		}
		return measuredAnswer(repairPlanReply(false)), nil
	})
	_, err := s.Turn(t.Context(), d)
	saved, ok, getErr := s.options.Records.Get(t.Context(), "drafts", draftID)
	if getErr != nil || !ok {
		t.Fatalf("保存記録: %v", getErr)
	}
	var current sessionState
	_ = json.Unmarshal(marshal(saved["aiSession"]), &current)
	if current.ID != "11111111-1111-4111-8111-111111111113" || current.EndedAt.IsZero() {
		t.Fatalf("検査で断ったターンの会話が残っていない: %+v", current)
	}
	return err
}

func TestReplyCheckFailedOnlyForCheckFailures(t *testing.T) {
	for _, kind := range []string{"reply_check", "proposal"} {
		if !replyCheckFailed(&ai.Failure{Kind: kind}) {
			t.Fatalf("%s not detected", kind)
		}
	}
	for _, err := range []error{nil, errors.New("x"), &ai.Failure{Kind: "timeout_idle"}, &ai.Failure{Kind: "format"}} {
		if replyCheckFailed(err) {
			t.Fatalf("detected %v", err)
		}
	}
}
