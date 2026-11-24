package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

func TestProviderWaitIsScopedBoundedAndCancelable(t *testing.T) {
	now := time.Unix(1000, 0)
	var delays []time.Duration
	s := &Service{options: Options{Now: func() time.Time { return now }}}
	s.options.Wait = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		now = now.Add(delay)
		return ctx.Err()
	}
	s.rateWaits.extend("codex", now.Add(time.Minute))
	if err := s.waitForProvider(t.Context(), Record{"id": "one"}, "claude"); err != nil || len(delays) != 0 {
		t.Fatal(err, delays)
	}
	other := &Service{options: s.options}
	if err := other.waitForProvider(t.Context(), Record{"id": "one"}, "codex"); err != nil || len(delays) != 0 {
		t.Fatal("別の実行器を止めた", err, delays)
	}
	if err := s.waitForProvider(t.Context(), Record{"id": "one"}, "codex"); err != nil || len(delays) != 1 || delays[0] < time.Minute || delays[0] >= time.Minute+2*time.Second {
		t.Fatal(err, delays)
	}
	if err := s.waitForProvider(t.Context(), Record{"id": "two"}, "codex"); err != nil || len(delays) != 1 {
		t.Fatal("期限後も待った", err, delays)
	}
	s.rateWaits.extend("codex", now.Add(time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.waitForProvider(ctx, Record{"id": "two"}, "codex"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProviderWaitHonorsExtendedLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Service{options: Options{Now: func() time.Time { return now }}}
	s.rateWaits.extend("codex", now.Add(time.Minute))
	waits := 0
	s.options.Wait = func(_ context.Context, delay time.Duration) error {
		waits++
		now = now.Add(delay)
		if waits == 1 {
			s.rateWaits.extend("codex", now.Add(3*time.Minute))
		}
		return nil
	}
	if err := s.waitForProvider(t.Context(), Record{"id": "one"}, "codex"); err != nil || waits != 2 {
		t.Fatal(err, waits)
	}
}

func TestNewConsultationWaitsOnlyAfterMeasuredRateLimit(t *testing.T) {
	for _, kind := range []string{"rate_limit", "authentication", "execution"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				calls++
				if calls == 1 {
					return measuredAnswer(nil), &ai.Failure{Kind: kind, Message: "fixture"}
				}
				return measuredAnswer(map[string]any{"ok": true}), nil
			})
			waits := 0
			s.options.Wait = func(context.Context, time.Duration) error { waits++; return nil }
			config, _ := s.options.Config(t.Context())
			settings := models.Settings{Provider: "codex", Model: "fixture"}
			_, _ = s.call(t.Context(), d, config, settings, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, nil, nil, nil)
			before := waits
			// A new call in the same account observes the earlier call's cooldown,
			// while authentication/execution failures do not create a shared pause.
			if _, err := s.call(t.Context(), d, config, settings, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			want := before
			if kind == "rate_limit" {
				want++
			}
			if waits != want {
				t.Fatal(kind, waits, want)
			}
		})
	}
}

func TestSharedProviderWaitRechecksUsageBeforeCalling(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(nil), nil })
	s.rateWaits.extend("codex", s.options.Now().Add(time.Minute))
	s.options.Wait = func(ctx context.Context, _ time.Duration) error {
		_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, r Record) (Record, error) {
			r["consultationBudget"] = Record{"calls": 1, "unknownCalls": 1}
			return r, nil
		})
		return err
	}
	config, _ := s.options.Config(t.Context())
	_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, nil, nil, nil)
	if !isMeasurementFailure(err) || calls != 0 {
		t.Fatal("待機中に発生した使用量不明を無視した", err, calls)
	}
}

// Exercise the actual call loop, including a persisted wait resumed after a
// restart. A different consultation extends the account deadline during the
// first wait; neither path may invoke the Runner at the obsolete deadline.
func TestCallRechecksExtendedProviderWaitBeforeEveryAttempt(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "再試行", true: "保存済み待機"}[resume], func(t *testing.T) {
			calls, waits := 0, 0
			var now, extended time.Time
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				calls++
				if !resume && calls == 1 {
					return measuredAnswer(nil), &ai.Failure{Kind: "rate_limit", Message: "fixture"}
				}
				if extended.IsZero() || now.Before(extended) {
					t.Fatalf("共有期限前に呼んだ: now=%s until=%s", now, extended)
				}
				return measuredAnswer(map[string]any{"ok": true}), nil
			})
			now = s.options.Now()
			s.options.Now = func() time.Time { return now }
			if resume {
				d["rateLimitWait"] = Record{"provider": "codex", "until": now.Add(time.Minute).Format(time.RFC3339Nano)}
			}
			s.options.Wait = func(_ context.Context, delay time.Duration) error {
				waits++
				if waits == 1 {
					extended = now.Add(3 * time.Minute)
					s.rateWaits.extend("codex", extended)
				}
				now = now.Add(delay)
				return nil
			}
			config, _ := s.options.Config(t.Context())
			_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, nil, nil, nil)
			wantCalls := 2
			if resume {
				wantCalls = 1
			}
			if err != nil || calls != wantCalls || waits != 2 {
				t.Fatal(err, calls, waits)
			}
		})
	}
}
