package startup

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/usage"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/runtimegate"
)

func reviewNotesApp(t *testing.T) *App {
	t.Helper()
	db := appPostgres(t)
	ctx, cancel := context.WithCancel(t.Context())
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	runtime := t.TempDir()
	sqlite := newAppStore(t)
	records, err := consultations.OpenRecords(ctx, sqlite.DB())
	if err != nil {
		t.Fatal(err)
	}
	gate, err := runtimegate.New(runtimegate.Options{RuntimeDir: runtime, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{ApplicationDir: root, RuntimeDir: runtime, Repositories: []string{"mofua-suki/issue-desk-intake"}, Values: map[string]any{"intake": map[string]any{}}}
	app := &App{Config: config, DB: db, SQLite: sqlite, Records: records, ctx: ctx, cancel: cancel, Gate: gate, Models: &models.Service{DB: db}, Fleet: groupTestSource(t, nil, true), Runner: &ai.Runner{}, Log: func(message string) { t.Log(message) }}
	t.Cleanup(app.Close)
	return app
}

func TestReviewNotesCycleAvoidsRepeatedDatabaseClaimPostgres(t *testing.T) {
	app := reviewNotesApp(t)
	if err := app.updateReviewNotes(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := app.DB.ExecContext(t.Context(), `CREATE FUNCTION reject_review_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected claim'; END $$;
CREATE TRIGGER reject_review_claim BEFORE INSERT OR UPDATE ON repo_note_processing FOR EACH ROW EXECUTE FUNCTION reject_review_claim()`)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.updateReviewNotes(t.Context()); err != nil {
		t.Fatalf("同日の周期がDBに再確認した: %v", err)
	}
	app.reviewNotesDates = reviewNotesAttempts{}
	if err := app.updateReviewNotes(t.Context()); err == nil || !strings.Contains(err.Error(), "unexpected claim") {
		t.Fatalf("再起動時はDBを確認すべき: %v", err)
	}
}

func writeReviewNotesAnswer(t *testing.T, p ai.ProcessRequest, raw string) {
	t.Helper()
	for i, arg := range p.Arguments {
		if arg == "-o" {
			if err := os.WriteFile(p.Arguments[i+1], []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("回答出力先がない")
}

func TestReviewNotesExistingCycleStartsAndCloseCancelsPostgres(t *testing.T) {
	app := reviewNotesApp(t)
	if _, err := app.DB.ExecContext(t.Context(), `INSERT INTO repo_notes(repo,kind,data) VALUES('mofua-suki/issue-desk-intake','finding','{"issue":1,"text":"入力検証が不足"}')`); err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	app.Runner.Execute = func(ctx context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
		if p.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ai.ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":1,"cached_input_tokens":0}}`)}, ctx.Err()
	}
	// This is the same existing immediate cycle and callback as startBackground.
	app.periodically("test-task", time.Minute, app.reconcileDailyNotes)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("起動時に処理されない")
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("停止時にキャンセルされない")
	}
	select {
	case <-closed:
		t.Fatal("実行中の処理を待たずに終了した")
	default:
	}
	release <- struct{}{}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("処理終了後も停止できない")
	}
	var raw []byte
	if err := app.DB.QueryRow(`SELECT json FROM usage_rows WHERE json::jsonb->>'step'=$1`, usage.ReviewNotesStep).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row["outcome"] != "failed" || row["inputTokens"] != float64(5) {
		t.Fatal(row)
	}
	var notes []byte
	var date string
	if err := app.DB.QueryRow(`SELECT notes,attempt_date FROM repo_note_processing WHERE repo='mofua-suki/issue-desk-intake'`).Scan(&notes, &date); err != nil || len(notes) != 0 || date == "" {
		t.Fatal(string(notes), date, err)
	}
}

func TestReviewNotesCapacityAndTemporarySwitchAfterWaitPostgres(t *testing.T) {
	app := reviewNotesApp(t)
	_, release, err := app.Gate.Acquire(t.Context(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	called := make(chan struct{}, 1)
	app.Runner.Execute = func(_ context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
		if p.Arguments[0] == "auth" {
			return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
		}
		args := strings.Join(p.Arguments, " ")
		if !strings.Contains(args, "--model haiku") {
			t.Fatal("注意点の更新の固定モデル（reviewNotes）と一時切り替えが反映されない", args)
		}
		called <- struct{}{}
		return ai.ProcessResult{Stdout: []byte(`{"type":"result","structured_output":{"assignments":[]},"usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"total_cost_usd":0.01}`)}, nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := app.answerReviewNotes(app.ctx, "mofua-suki/issue-desk-intake", "分類対象")
		finished <- err
	}()
	select {
	case <-called:
		t.Fatal("既存のAI枠を無視した")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := app.Models.SetDisable(t.Context(), "codex", map[string]any{}, "test", []map[string]any{{"id": "codex", "ready": true}, {"id": "claude", "ready": true}}); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("枠の解放後に進まない")
	}
	var raw []byte
	if err := app.DB.QueryRow(`SELECT json FROM usage_rows WHERE json::jsonb->>'step'=$1`, usage.ReviewNotesStep).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row["provider"] != "claude" || row["model"] != "haiku" || row["outcome"] != "completed" || row["inputTokens"] != float64(2) {
		t.Fatal(row)
	}
}

func TestReviewNotesRunnerFailuresRecordUsagePostgres(t *testing.T) {
	for _, failure := range []string{"execution", "timeout", "schema"} {
		t.Run(failure, func(t *testing.T) {
			app := reviewNotesApp(t)
			app.Runner.Execute = func(_ context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
				if p.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				result := ai.ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":2,"cached_input_tokens":0}}`)}
				switch failure {
				case "execution":
					result.Code = 1
				case "timeout":
					return result, context.DeadlineExceeded
				case "schema":
					writeReviewNotesAnswer(t, p, `{"assignments":"不正"}`)
				}
				return result, nil
			}
			_, err := app.answerReviewNotes(app.ctx, "mofua-suki/issue-desk-intake", "分類対象")
			if err == nil || failure == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			var raw []byte
			if err := app.DB.QueryRow(`SELECT json FROM usage_rows WHERE json::jsonb->>'step'=$1`, usage.ReviewNotesStep).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if row["outcome"] != "failed" || row["inputTokens"] != float64(7) {
				t.Fatal(row)
			}
		})
	}
}

func TestReviewNotesDoesNotBlockRecoveryCyclePostgres(t *testing.T) {
	for _, stage := range []string{"capacity", "execution"} {
		t.Run(stage, func(t *testing.T) {
			app := reviewNotesApp(t)
			if _, err := app.DB.ExecContext(t.Context(), `INSERT INTO repo_notes(repo,kind,data) VALUES('mofua-suki/issue-desk-intake','finding','{"issue":1,"text":"入力検証が不足"}')`); err != nil {
				t.Fatal(err)
			}
			if stage == "capacity" {
				_, release, err := app.Gate.Acquire(t.Context(), "ai")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			started := make(chan struct{}, 1)
			app.Runner.Execute = func(ctx context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
				if p.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				started <- struct{}{}
				<-ctx.Done()
				return ai.ProcessResult{}, ctx.Err()
			}
			cycles := make(chan error, 20)
			// Same registered callback as production, with only its interval
			// shortened. Each completed cycle includes real recoverSubmissions.
			app.periodically("test-task", 10*time.Millisecond, func(ctx context.Context) error {
				err := app.reconcileDailyNotes(ctx)
				select {
				case cycles <- err:
				default:
				}
				return err
			})
			deadline := time.After(5 * time.Second)
			for {
				var date string
				err := app.DB.QueryRowContext(t.Context(), `SELECT attempt_date FROM repo_note_processing WHERE repo='mofua-suki/issue-desk-intake'`).Scan(&date)
				if err == nil && date != "" {
					break // The worker reached capacity wait, or went past it.
				}
				select {
				case <-deadline:
					t.Fatal("日次処理が始まらない", err)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if stage == "execution" {
				select {
				case <-started:
				case <-deadline:
					t.Fatal("AIが始まらない")
				}
			}
			// Discard earlier callbacks, then require subsequent timer ticks
			// while the daily worker is still waiting or executing.
			for len(cycles) > 0 {
				<-cycles
			}
			for range 3 {
				select {
				case err := <-cycles:
					if err != nil || !app.reviewNotesRunning.Load() {
						t.Fatal("日次処理の待機中に照合を続けられない", err)
					}
				case <-deadline:
					t.Fatal("AI待機・実行が照合周期を止めた")
				}
			}
			if stage == "capacity" && len(started) != 0 {
				t.Fatal("AI枠を無視して実行した")
			}
			app.Close()
			if app.reviewNotesRunning.Load() {
				t.Fatal("停止時に日次処理の終了を待たない")
			}
		})
	}
}
