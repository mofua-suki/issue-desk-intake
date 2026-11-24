package consultationai

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
)

// A consultation saved while it still waited for the removed tier classifier
// (tier.pending=true) must not stop or call a classifier: its turn runs at the
// recorded heavy level, and the saved record is kept readable as it was.
func TestLegacyPendingTierRunsWithoutClassifier(t *testing.T) {
	var steps []string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		steps = append(steps, r.Step)
		return measuredAnswer(reply()), nil
	})
	d["tier"] = Record{"level": "heavy", "source": "default", "reason": "始めの判定の前です（判定が済むまで難易度「高」）", "pending": true, "attempts": 1}
	if _, err := s.options.Records.Change(t.Context(), "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return copyRecord(d), nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 {
		t.Fatal("相談のAIが呼ばれない")
	}
	for _, step := range steps {
		if step == usage.TierClassificationStep {
			t.Fatalf("廃止した始めの判定を呼んだ: %v", steps)
		}
	}
	saved, _, err := s.options.Records.Get(t.Context(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	tier := fleet.Object(saved["tier"])
	if tier["level"] != "heavy" || tier["source"] != "default" {
		t.Fatalf("古い難易度の記録が変わった: %v", tier)
	}
}
