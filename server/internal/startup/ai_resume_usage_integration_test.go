package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

// The process is simulated, but the consultation, Runner, usage validation and
// PostgreSQL storage are the same paths used by the application.
func TestRecordUsageResumedConsultation(t *testing.T) {
	const session = "12345678-1234-1234-1234-123456789abc"
	const draftID = "11111111-1111-4111-8111-111111111111"
	for _, known := range []bool{true, false} {
		name := "missing baseline"
		if known {
			name = "measured delta"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			db := appPostgres(t)
			local := newAppStore(t)
			records, err := consultations.OpenRecords(ctx, local.DB())
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			home := filepath.Join(dir, "codex")
			t.Setenv("CODEX_HOME", home)
			logDir := filepath.Join(home, "sessions", "2026", "11", "04")
			if err := os.MkdirAll(logDir, 0700); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(logDir, "rollout-2026-11-04-"+session+".jsonl")
			token := func(at time.Time, in, cache, out int) string {
				return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":%d,\"cached_input_tokens\":%d,\"output_tokens\":%d}}}}\n", at.UTC().Format(time.RFC3339Nano), in, cache, out)
			}
			response := `{"response":{"executionPlan":null,"completionPlan":null,"reply":"質問です","basisConflicts":[],"title":"","body":"","ready":false,"readFiles":[],"reviewedFiles":[],"questions":["確認してください"],"imageReviews":[],"splitIssues":[],"crossProject":{"reason":"","scopes":[],"approvalMessageId":""},"changeScope":{"known":false,"paths":[],"reason":""},"dependsOnIssues":[],"kind":"implementation","escalate":false,"escalateReason":"","heavyReason":"","workType":"bugfix","estimate":{"lines":0,"files":0}}}`
			calls, resumes := 0, 0
			runner := &ai.Runner{Execute: func(_ context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
				if p.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				resumed := strings.Contains(strings.Join(p.Arguments, " "), "resume "+session)
				if resumed {
					resumes++
					if known {
						f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := f.WriteString(strings.TrimSuffix(token(time.Now(), 1500, 1200, 80), "\n")); err != nil {
							t.Fatal(err)
						}
						if err := f.Close(); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", session) + token(time.Now().Add(-time.Second), 900, 700, 50)
					if err := os.WriteFile(logPath, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				for i, arg := range p.Arguments {
					if arg == "-o" {
						if err := os.WriteFile(p.Arguments[i+1], []byte(response), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				input, cache, output := 900, 700, 50
				if resumed {
					input, cache, output = 1500, 1200, 80
				}
				stdout := fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":%q}\n{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":%d,\"cached_input_tokens\":%d,\"output_tokens\":%d}}\n", session, input, cache, output)
				return ai.ProcessResult{Stdout: []byte(stdout)}, nil
			}}
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/repos/owner/repo"):
					fmt.Fprint(w, `{"default_branch":"trunk"}`)
				case strings.Contains(r.URL.Path, "/branches/"):
					fmt.Fprintf(w, `{"commit":{"id":%q}}`, strings.Repeat("a", 40))
				case strings.Contains(r.URL.Path, "/git/trees/"):
					fmt.Fprint(w, `{"tree":[],"truncated":false}`)
				case strings.HasSuffix(r.URL.Path, "/commits"):
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected fixture request: %s", r.URL.Path)
					http.Error(w, "missing", 404)
				}
			}))
			defer fixture.Close()
			tokenPath := filepath.Join(dir, "fixture-token")
			if err := os.WriteFile(tokenPath, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := gitea.New(fixture.URL, tokenPath, fixture.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			config := map[string]any{"model": "fixture", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"proxy": map[string]any{"enabled": false}}}
			schema, err := filepath.Abs(filepath.Join("..", "..", "schemas"))
			if err != nil {
				t.Fatal(err)
			}
			app := &App{DB: db}
			service, err := consultationai.New(consultationai.Options{Config: func(context.Context) (map[string]any, error) { return config, nil }, Models: &models.Service{DB: db}, Runner: runner, Gitea: client, Records: records, RuntimeDir: dir, SchemaDir: schema, RecordUsage: app.recordUsage})
			if err != nil {
				t.Fatal(err)
			}
			draft := consultations.Record{"id": draftID, "project": "owner/repo", "status": "thinking", "run": "run", "revision": 0, "tier": map[string]any{"level": "fixed"}, "messages": []any{map[string]any{"role": "user", "id": "u1", "text": "依頼"}}}
			_, err = records.Change(ctx, "drafts", draftID, func(*sql.Tx, consultations.Record) (consultations.Record, error) { return draft, nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Turn(ctx, draft); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			saved, ok, err := records.Get(ctx, "drafts", draftID)
			if err != nil || !ok {
				t.Fatalf("draft: %v %v", ok, err)
			}
			saved["messages"] = append(saved["messages"].([]any), map[string]any{"role": "user", "id": "u2", "text": "続き"})
			if _, err := records.Change(ctx, "drafts", draftID, func(*sql.Tx, consultations.Record) (consultations.Record, error) { return saved, nil }); err != nil {
				t.Fatal(err)
			}
			if !known {
				if err := os.Remove(logPath); err != nil {
					t.Fatal(err)
				}
			}
			_, turnErr := service.Turn(ctx, saved)
			if known && turnErr != nil {
				t.Fatalf("resumed: %v", turnErr)
			}
			if !known && turnErr == nil {
				t.Fatal("missing usage did not hold consultation")
			}
			if !known {
				held, ok, err := records.Get(ctx, "drafts", draftID)
				if err != nil || !ok {
					t.Fatalf("held draft: %v %v", ok, err)
				}
				hold, _ := held["usageMeasurementHold"].(map[string]any)
				if hold["reason"] != "missing" || hold["recordCause"] != nil {
					t.Fatalf("unexpected hold: %+v", hold)
				}
			}
			if calls != 2 || resumes != 1 {
				t.Fatalf("calls=%d resumed=%d", calls, resumes)
			}
			rows, skipped, err := (&usage.Store{DB: db}).Read(ctx, "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			if err != nil || skipped != 0 || len(rows) != 2 {
				t.Fatalf("rows=%v skipped=%d err=%v", rows, skipped, err)
			}
			var row map[string]any
			for _, r := range rows {
				if r["sessionMode"] == "resumed" {
					row = r
				}
			}
			if row == nil || !usage.ValidUsage(row) || row["step"] != usage.DraftStep || row["outcome"] != "completed" {
				t.Fatalf("invalid resumed row: %+v", row)
			}
			reasons, ok := row["measurementMissingReasons"].(map[string]any)
			if !ok {
				t.Fatalf("missing input measurement: %+v", row)
			}
			if row["promptTotalBytes"] == nil || reasons["skill"] != "not_observed" {
				t.Fatalf("input measurement: %+v", row)
			}
			for key, expected := range map[string]float64{"inputTokens": 600, "cacheReadTokens": 500, "outputTokens": 30} {
				value, exists := row[key]
				if !exists || known && value != expected || !known && value != nil {
					t.Errorf("%s=%v exists=%v", key, value, exists)
				}
			}
			if !known && (reasons["contextMaxTokens"] != "provider_unavailable" || reasons["contextTotalTokens"] != "provider_unavailable") {
				raw, _ := json.Marshal(row)
				t.Fatalf("missing reasons: %s", raw)
			}
		})
	}
}
