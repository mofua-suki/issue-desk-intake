package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

func TestTurnNamesTheStageOfTheFailure(t *testing.T) {
	s, d := testService(t, nil)
	d["run"], d["status"] = "run", "thinking"
	s.options.Config = func(context.Context) (map[string]any, error) {
		return nil, errors.New("config unreadable")
	}
	_, err := s.Turn(t.Context(), d)
	if err == nil || consultations.FailureStage(err) != "設定の読取" || !strings.Contains(err.Error(), "config unreadable") {
		t.Fatalf("段階か原因が違います: %v %q", err, consultations.FailureStage(err))
	}
}

func TestMeasurementHoldKeepsRecordAndCallCauses(t *testing.T) {
	const secret = "fixture-secret-value"
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		return measuredAnswer(nil), errors.New("call broke token=" + secret)
	})
	s.options.RecordUsage = func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error {
		return errors.New("usage table locked password=" + secret)
	}
	config, _ := s.options.Config(t.Context())
	_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, nil, nil, nil)
	if !isMeasurementFailure(err) {
		t.Fatal(err)
	}
	hold := fleet.Object(d["usageMeasurementHold"])
	if !strings.Contains(fleet.Text(hold["recordCause"]), "usage table locked") || !strings.Contains(fleet.Text(hold["originalCause"]), "call broke") {
		t.Fatalf("保留に元の失敗がありません: %+v", hold)
	}
	for _, want := range []string{"使用量の記録を保存できなかった", "usage table locked", "call broke"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("停止の文に %q がありません: %s", want, err)
		}
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(fleet.Text(hold["recordCause"]), secret) || strings.Contains(fleet.Text(hold["originalCause"]), secret) {
		t.Fatalf("資格情報が残っています: %s %+v", err, hold)
	}
}
