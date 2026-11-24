package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
)

func TestInputCheckBoundaries(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	schema := filepath.Join(t.TempDir(), "schema")
	if err := os.WriteFile(schema, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		config      map[string]any
		input, full string
		tooLarge    bool
	}{
		{"default chars exact", nil, strings.Repeat("a", 524285), strings.Repeat("a", 524285), false},
		{"default chars over", nil, strings.Repeat("a", 524286), strings.Repeat("a", 524286), true},
		{"default bytes exact", nil, strings.Repeat("界", 349524) + "a", strings.Repeat("界", 349524) + "a", false},
		{"default bytes over", nil, strings.Repeat("界", 349525), strings.Repeat("界", 349525), true},
		{"configured exact", map[string]any{"intake": map[string]any{"maxInputChars": 7, "maxInputBytes": 11}}, "界界aa", "界界aa", false},
		{"configured chars over", map[string]any{"intake": map[string]any{"maxInputChars": 6, "maxInputBytes": 100}}, "界界aa", "界界aa", true},
		{"configured bytes over", map[string]any{"intake": map[string]any{"maxInputChars": 100, "maxInputBytes": 10}}, "界界aa", "界界aa", true},
		{"resumed whole over", map[string]any{"intake": map[string]any{"maxInputChars": 7, "maxInputBytes": 100}}, "a", "aaaaa", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.inputCheck(context.Background(), d, tc.config, ai.Request{SchemaFile: schema, Input: tc.input, Resume: true}, tc.full)
			var failure *ai.Failure
			if tc.tooLarge {
				if !errors.As(err, &failure) || failure.DeskCode != "INPUT_TOO_LARGE" {
					t.Fatalf("expected input limit, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInputOverflowCompactsThenSplitsFiveFiles(t *testing.T) {
	files := "src/a.go src/b.go src/c.go src/d.go src/e.go"
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if !strings.Contains(r.Input, "資料本文を読まず") {
			t.Fatalf("unexpected call: %s", r.Step)
		}
		return measuredAnswer(map[string]any{"parts": []any{files, "別の要件"}}), nil
	})
	baseConfig := s.options.Config
	s.options.Config = func(ctx context.Context) (map[string]any, error) {
		c, err := baseConfig(ctx)
		fleet.Object(c["intake"])["maxInputChars"] = 8000
		fleet.Object(c["intake"])["maxInputBytes"] = 100000
		return c, err
	}
	d["messages"] = []any{Record{"role": "user", "id": "u", "text": files + " 目的と受入条件を保つ。" + strings.Repeat("依頼", 2500)}}
	split := false
	s.options.Split = func(_ context.Context, _ Record, parts []string) (Record, error) {
		split = true
		if len(parts) != 2 || parts[0] != files {
			t.Fatalf("parts: %v", parts)
		}
		return Record{"recovered": true}, nil
	}
	got, err := s.Turn(context.Background(), d)
	if err != nil || !split || got["recovered"] != true || calls != 1 {
		t.Fatalf("result=%v split=%v calls=%d err=%v", got, split, calls, err)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	records := fleet.Elements(fleet.Object(saved["inputBudget"])["records"])
	if len(records) == 0 {
		t.Fatal("missing input check")
	}
	fullChars := func(item any) float64 {
		return fleet.Number(fleet.Object(fleet.Object(fleet.Object(item)["full"])["total"])["chars"])
	}
	if len(records) < 2 || fullChars(records[len(records)-2]) <= 8000 || fullChars(records[len(records)-1]) > 8000 {
		t.Fatalf("超過した通常入力と上限内の分割入力の両方を記録していません: %v", records)
	}
}

func TestInputSplitRecoversMeasuredUsageAfterInterruptedBudgetWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		calls++
		if !strings.Contains(req.Input, "資料本文を読まず") || req.Resume {
			t.Fatal("通常の相談セッションを分割に再利用しました")
		}
		return measuredAnswer(map[string]any{"parts": []any{"src/a.go", "別の要件"}}), nil
	})
	config, err := s.options.Config(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	intake := fleet.Object(config["intake"])
	intake["maxInputChars"] = 8000
	intake["consultationBudget"] = Record{"calls": 1, "weighted": 100000}
	d["messages"] = []any{Record{"role": "user", "id": "u", "text": "src/a.go " + strings.Repeat("依頼", 2500)}}
	if err := s.save(t.Context(), d, Record{"messages": d["messages"]}); err != nil {
		t.Fatal(err)
	}
	var row Record
	s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
		row = Record{"callId": req.CallID, "draft": req.Draft, "repo": req.Project, "source": "intake",
			"provider": req.Provider, "inputTokens": float64(*u.InputTokens), "outputTokens": float64(*u.OutputTokens), "cacheReadTokens": float64(*u.CacheReadTokens)}
		// Stop the turn after the independent usage store has accepted the row,
		// before the draft can settle its saved reservation.
		cancel()
		return nil
	}
	s.options.ReadUsage = func(_ context.Context, id string) (map[string]any, error) {
		if row["callId"] == id {
			return row, nil
		}
		return nil, nil
	}
	s.options.Split = func(context.Context, Record, []string) (Record, error) {
		t.Fatal("使用量が確定する前に子相談を作成しました")
		return nil, nil
	}
	if _, err := s.Turn(ctx, d); !errors.Is(err, context.Canceled) || calls != 1 || row["draft"] != draftID {
		t.Fatalf("分割AIの中断と使用量を確認できません: calls=%d usage=%v err=%v", calls, row, err)
	}
	readDraft := func() Record {
		t.Helper()
		saved, found, err := s.options.Records.Get(t.Context(), "drafts", draftID)
		if err != nil || !found {
			t.Fatalf("相談の読取: found=%v err=%v", found, err)
		}
		return saved
	}
	saved := readDraft()
	pending := fleet.Object(fleet.Object(saved["consultationBudget"])["pendingCall"])
	if pending["id"] != row["callId"] {
		t.Fatalf("分割の予約と保存した使用量が一致しません: %v %v", pending, row)
	}
	for i := 0; i < 2; i++ {
		result, err := s.Turn(t.Context(), readDraft())
		budget := fleet.Object(result["consultationBudget"])
		settlements := fleet.Elements(budget["measurementReconciliations"])
		if err != nil || calls != 1 || fleet.Object(budget["stopped"]) == nil || budget["pendingCall"] != nil ||
			fleet.Number(budget["weighted"]) <= 0 || fleet.Number(budget["unknownCalls"]) != 0 ||
			len(settlements) != 1 || fleet.Object(settlements[0])["callId"] != row["callId"] {
			t.Fatalf("再開で一回だけ照合して上限で停止できません: calls=%d budget=%v err=%v", calls, budget, err)
		}
	}
}

func TestTurnResumesSavedSplitWithOnlySomeChildrenCreated(t *testing.T) {
	s, parent := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("保存済みの分割をAIへ送り直しました")
		return ai.Answer{}, nil
	})
	children := []any{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}
	parts := []any{"保存済みの子A", "保存済みの子B"}
	if err := s.save(t.Context(), parent, Record{"consultationChildren": children, "consultationParts": parts}); err != nil {
		t.Fatal(err)
	}
	first := childConsultation(parent, children[0].(string), parts[0].(string), "existing", "2026-11-01T00:00:00Z")
	first["status"], first["body"] = "draft", "作成後の相談内容"
	if _, err := s.options.Records.Change(t.Context(), "drafts", children[0].(string), func(*sql.Tx, Record) (Record, error) {
		return first, nil
	}); err != nil {
		t.Fatal(err)
	}
	saved, found, err := s.options.Records.Get(t.Context(), "drafts", draftID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	s.options.Split = s.SplitChildren
	result, err := s.Turn(t.Context(), saved)
	if err != nil || string(marshal(result["consultationChildren"])) != string(marshal(children)) {
		t.Fatalf("分割を再開できません: result=%v err=%v", result, err)
	}
	for i, id := range children {
		child, found, err := s.options.Records.Get(t.Context(), "drafts", id.(string))
		if err != nil || !found || child["parentConsultation"] != draftID {
			t.Fatalf("子相談 %d が見つかりません: %v %v", i, child, err)
		}
		firstMessage := fleet.Object(fleet.Elements(child["messages"])[0])
		if firstMessage["text"] != parts[i] || i == 0 && (child["body"] != first["body"] || firstMessage["id"] != "existing") {
			t.Fatalf("既存の子相談の内容を保持していません: %v", child)
		}
	}
}

func TestRecoveryPartsPreserveOtherGuards(t *testing.T) {
	files := "src/a.go src/b.go src/c.go src/d.go src/e.go"
	source := files + " " + strings.Repeat("x", 9000)
	for _, tc := range []struct {
		name  string
		parts []string
		valid bool
	}{
		{"five files", []string{files, "別の要件"}, true},
		{"missing", []string{"src/a.go", "別の要件"}, false},
		{"additional", []string{files + " src/new.go", "別の要件"}, false},
		{"empty", []string{files, " "}, false},
		{"too long", []string{files + strings.Repeat("x", 8001), "別の要件"}, false},
		{"not smaller", []string{source, "別の要件"}, false},
		{"one part", []string{files}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateParts(tc.parts, source); (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
