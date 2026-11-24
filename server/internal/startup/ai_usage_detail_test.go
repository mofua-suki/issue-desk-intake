package startup

import (
	"encoding/json"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
)

func TestRecordUsagePersistsEffectiveTier(t *testing.T) {
	db := appPostgres(t)
	for _, tier := range []string{"light", "standard", "heavy", ""} {
		t.Run("tier="+tier, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM usage_rows`); err != nil {
				t.Fatal(err)
			}
			req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1, Tier: tier}
			if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, time.Second, true); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			got, exists := row["tier"]
			if (tier == "" && exists) || (tier != "" && (!exists || got != tier)) || row["source"] != "intake" {
				t.Fatalf("保存された行: %s", raw)
			}
		})
	}
}

func TestRecordUsagePersistsDetailFields(t *testing.T) {
	db := appPostgres(t)
	zero, one := int64(0), int64(1)
	u := ai.Usage{Calls: &one, ContextMaxTokens: &zero, ContextTotalTokens: &zero, PromptTotalBytes: &one, PromptBytes: map[string]*int64{"conversation": &one, "initialDocuments": nil}, MeasurementMissingReasons: map[string]string{"initialDocuments": "documents_source_unknown"}, ContextReads: map[string]any{"requested": 1, "repeated": 0, "files": []any{map[string]any{"repo": "owner/app", "path": "README.md", "requested": 1, "repeated": 0}}}}
	req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1}
	if err := (&App{DB: db}).recordUsage(t.Context(), req, u, time.Second, true); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row["calls"] != float64(1) || row["contextMaxTokens"] != float64(0) || row["promptTotalBytes"] != float64(1) || row["promptBytes"].(map[string]any)["initialDocuments"] != nil || row["measurementMissingReasons"].(map[string]any)["initialDocuments"] != "documents_source_unknown" || row["contextReads"].(map[string]any)["requested"] != float64(1) {
		t.Fatalf("保存された行: %s", raw)
	}
}

func TestRecordUsageNumericFieldsPersist(t *testing.T) {
	db := appPostgres(t)
	req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 2, Turn: 3, CallTrigger: "failure_retry"}
	if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, 1234*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if !usage.ValidUsage(row) || row["attempt"] != float64(2) || row["turn"] != float64(3) || row["elapsedMs"] != float64(1234) || row["callTrigger"] != "failure_retry" {
		t.Fatalf("保存された行: %s", raw)
	}
}

func TestRecordUsageCallTrigger(t *testing.T) {
	db := appPostgres(t)
	triggers := []string{"", "initial", "user_reply", "context_read", "automatic_continuation", "failure_retry", "recovery_continuation", "ai_message_continuation", "format_repair", "rate_limit_retry", "filing_recompare", "unknown"}
	for _, trigger := range triggers {
		t.Run("trigger="+trigger, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM usage_rows`); err != nil {
				t.Fatal(err)
			}
			req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1, CallTrigger: trigger}
			if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, time.Second, true); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if !usage.ValidUsage(row) {
				t.Fatalf("保存された行が不正: %s", raw)
			}
			got, exists := row["callTrigger"]
			if trigger == "" || trigger == "unknown" {
				if exists {
					t.Fatalf("callTrigger は省略されるべき: %s", raw)
				}
			} else if !exists || got != trigger {
				t.Fatalf("callTrigger が一致しない: %s", raw)
			}
		})
	}
}

func TestRecordUsageOmitsCallTriggerForExcludedSteps(t *testing.T) {
	db := appPostgres(t)
	for _, step := range []string{usage.ReviewNotesStep, usage.TierClassificationStep, usage.FilingScopeStep} {
		t.Run(step, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM usage_rows`); err != nil {
				t.Fatal(err)
			}
			req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: step, Attempt: 1, Turn: 1, CallTrigger: "initial"}
			if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, time.Second, true); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if !usage.ValidUsage(row) || row["step"] != req.Step {
				t.Fatalf("保存された行が不正: %s", raw)
			}
			if _, exists := row["callTrigger"]; exists {
				t.Fatalf("対象外ステップの callTrigger は省略されるべき: %s", raw)
			}
		})
	}
}

func TestRecordUsageRepairDetails(t *testing.T) {
	db := appPostgres(t)
	req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1, CallTrigger: "format_repair", RepairKind: "draft_format", RepairField: "title"}
	if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, time.Second, true); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if !usage.ValidUsage(row) || row["repairKind"] != req.RepairKind || row["repairField"] != req.RepairField {
		t.Fatalf("saved: %s", raw)
	}
}

func TestRecordUsageRejectsInvalidRepairDetail(t *testing.T) {
	db := appPostgres(t)
	req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1, CallTrigger: "format_repair", RepairKind: "draft_format", RepairField: "invalid field"}
	if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, time.Second, true); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if !usage.ValidUsage(row) || row["callTrigger"] != "format_repair" || row["repairKind"] != nil || row["repairField"] != nil {
		t.Fatalf("saved: %s", raw)
	}
}

func TestRecordUsageDiagnosticJSONAndRead(t *testing.T) {
	db := appPostgres(t)
	req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1}
	for i, kind := range []string{"completed", "timeout_idle", ""} {
		req.Turn = i + 1
		if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{DiagnosticKind: kind}, time.Second, i == 0); err != nil {
			t.Fatal(err)
		}
	}
	rows, skipped, err := (&usage.Store{DB: db}).Read(t.Context(), "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || skipped != 0 || len(rows) != 3 {
		t.Fatalf("Read rows=%d skipped=%d err=%v", len(rows), skipped, err)
	}
	found := map[float64]map[string]any{}
	for _, row := range rows {
		found[row["turn"].(float64)] = row
	}
	for i, want := range []string{"completed", "timeout_idle", ""} {
		row := found[float64(i+1)]
		got, exists := row["diagnosticKind"]
		if exists != (want != "") || (want != "" && got != want) {
			t.Errorf("turn %d: %#v", i+1, row)
		}
	}
}

func TestRecordUsageSessionModes(t *testing.T) {
	db := appPostgres(t)
	const id = "22222222-2222-4222-8222-222222222222"
	cases := []struct {
		name, provider  string
		persist, resume bool
		session, want   string
	}{
		{"once", "codex", false, false, "", "once"},
		{"new", "codex", true, false, "", "new"},
		{"new assigned ID", "claude", true, false, id, "new"},
		{"resumed", "codex", true, true, id, "resumed"},
		{"retry new", "codex", true, false, "", "new"},
		{"missing ID", "codex", true, true, "", ""},
		{"invalid ID", "codex", true, true, "invalid", ""},
		{"ephemeral resume", "codex", false, true, id, ""},
		{"ephemeral ID", "claude", false, false, id, ""},
		{"unknown provider", "other", true, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM usage_rows`); err != nil {
				t.Fatal(err)
			}
			req := ai.Request{Provider: tc.provider, Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1, PersistSession: tc.persist, Resume: tc.resume, Session: tc.session}
			if tc.name == "retry new" {
				req.Attempt = 2
				req.CallTrigger = "failure_retry"
			}
			if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{}, time.Second, true); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if !usage.ValidUsage(row) || (tc.want == "" && row["sessionMode"] != nil) || (tc.want != "" && row["sessionMode"] != tc.want) || row["sessionCall"] != nil || row["modelCalls"] != nil {
				t.Fatalf("saved: %s", raw)
			}
			if tc.name == "retry new" && row["callTrigger"] != "failure_retry" {
				t.Fatalf("saved: %s", raw)
			}
		})
	}
}

func TestRecordUsageModelCallsFromEvents(t *testing.T) {
	db := appPostgres(t)
	cases := []struct {
		name, provider, output string
		success                bool
		calls, input           any
	}{
		{"completed turns", "codex", "{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"cached_input_tokens\":1}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"cached_input_tokens\":0}}", true, float64(2), float64(8)},
		{"failed", "codex", `{"type":"turn.failed","modelCalls":2}`, false, float64(0), nil},
		{"interrupted after completion", "codex", "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":5}}\n{\"type\":\"turn.started\"}", false, float64(1), float64(5)},
		{"truncated output", "codex", "{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\",\"usage\":", false, float64(0), nil},
		{"invalid counts", "codex", `{"type":"turn.completed","modelCalls":-1,"usage":{"input_tokens":-1,"model_calls":0}}`, true, float64(1), nil},
		{"unsubstantiated positive count", "codex", `{"type":"turn.completed","modelCalls":4,"usage":{"input_tokens":7,"model_calls":4}}`, true, float64(1), float64(7)},
		{"other provider", "claude", `{"type":"result","num_turns":2,"modelCalls":3,"usage":{"input_tokens":9}}`, true, float64(2), float64(9)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM usage_rows`); err != nil {
				t.Fatal(err)
			}
			req := ai.Request{Provider: tc.provider, Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1, CallTrigger: "initial"}
			measured := ai.Measure(tc.provider, []byte(tc.output))
			if err := (&App{DB: db}).recordUsage(t.Context(), req, measured, time.Second, tc.success); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if !usage.ValidUsage(row) || row["modelCalls"] != nil || row["calls"] != tc.calls || row["inputTokens"] != tc.input || row["callTrigger"] != "initial" || row["sessionMode"] != "once" {
				t.Fatalf("saved: %s", raw)
			}
		})
	}
}

func TestRecordUsagePersistsPhysicalCallIDs(t *testing.T) {
	db := appPostgres(t)
	a := &App{DB: db}
	ids := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}
	for _, id := range ids {
		req := ai.Request{CallID: id, Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1}
		if err := a.recordUsage(t.Context(), req, ai.Usage{}, time.Second, false); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := (&usage.Store{DB: db}).Read(t.Context(), "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r["callId"].(string)] = true
		if value, ok := r["inputTokens"]; ok && value != nil {
			t.Fatalf("unknown changed: %v", r)
		}
	}
	if !seen[ids[0]] || !seen[ids[1]] {
		t.Fatal(rows)
	}
}
