package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

func TestConsultationUsageSavedAndReloaded(t *testing.T) {
	db := budgetAlertPostgres(t)
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		u := ai.Measure("codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}}`))
		return ai.Answer{Usage: u, Value: map[string]any{"ok": true}}, nil
	})
	d["messages"] = []any{Record{"text": "秘密の会話"}}
	d["body"] = "秘密の本文"
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: []Document{{Project: "owner/app", Path: "README.md", Text: "秘密の資料", Size: 15}}}}}}
	data, _, _ := m.data(false)
	input := prompt(d, data, nil, 1, nil, "")
	s.options.RecordUsage = func(ctx context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
		b, e := json.Marshal(u)
		if e != nil {
			return e
		}
		row := map[string]any{}
		if e = json.Unmarshal(b, &row); e != nil {
			return e
		}
		row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
		row["step"], row["provider"], row["model"] = req.Step, req.Provider, req.Model
		counts, e := (&usage.Store{DB: db}).AddRows(ctx, "intake", []map[string]any{row})
		if e != nil {
			return e
		}
		if counts.Added != 1 {
			t.Fatalf("保存失敗: %+v", counts)
		}
		return nil
	}
	config, _ := s.options.Config(t.Context())
	doc := m.state.Projects["owner/app"].Reads[0]
	ctx := t.Context()
	_, err := s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, filepath.Join(s.options.SchemaDir, "intake.schema.json"), input, input, nil, nil, nil, map[string]bool{documentKey(doc): true})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT json FROM usage_rows`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatal(err)
	}
	if row["calls"] != float64(1) || row["contextMaxTokens"] != float64(0) || row["contextTotalTokens"] != float64(0) || row["promptTotalBytes"] != float64(len(input)) {
		t.Fatalf("計測: %s", raw)
	}
	parts := row["promptBytes"].(map[string]any)
	reasons := row["measurementMissingReasons"].(map[string]any)
	schema, err := os.ReadFile(filepath.Join(s.options.SchemaDir, "intake.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 14 || parts["conversation"] != float64(len(marshal(d["messages"]))) || parts["initialDocuments"] != float64(len(marshal(doc))) || parts["additionalDocuments"] != float64(0) || parts["schema"] != float64(len(schema)) || !usage.ValidUsage(row) {
		t.Fatalf("内訳: %s", raw)
	}
	for name, value := range parts {
		if value == nil && reasons[name] == nil {
			t.Fatalf("欠測理由なし: %s", name)
		}
	}
	reads := row["contextReads"].(map[string]any)
	paths := reads["files"].([]any)
	if reads["requested"] != float64(1) || paths[0].(map[string]any)["path"] != "README.md" {
		t.Fatalf("資料: %s", raw)
	}
	for _, secret := range []string{"秘密の会話", "秘密の本文", "秘密の資料"} {
		if strings.Contains(raw, secret) {
			t.Fatal("内容を保存した")
		}
	}
}

func TestResumedTimeoutRetryMeasuresEachActualInput(t *testing.T) {
	calls := 0
	inputs := []string{}
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		inputs = append(inputs, req.Input)
		calls++
		u := ai.Measure("codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":1,"cached_input_tokens":0}}`))
		if calls == 1 {
			return ai.Answer{Usage: u}, measuredTimeout{}
		}
		return ai.Answer{Usage: u, Value: map[string]any{"ok": true}}, nil
	})
	doc := Document{Project: "owner/app", Path: "README.md", Text: "資料本文"}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: []Document{doc}}}}}
	data, shown, _ := m.data(false)
	full := prompt(d, data, nil, 1, nil, "")
	input := resumedPrompt(full, d, m, &sessionState{ID: "session", Sent: map[string]bool{}}, shown, turnPromptState{})
	rows := []ai.Usage{}
	s.options.RecordUsage = func(_ context.Context, _ ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
		rows = append(rows, u)
		return nil
	}
	config, _ := s.options.Config(t.Context())
	ctx := t.Context()
	_, err := s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, filepath.Join(s.options.SchemaDir, "intake.schema.json"), input, full, nil, nil, nil, map[string]bool{documentKey(doc): true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || len(inputs) != 2 || inputs[0] != input || inputs[1] != full || *rows[0].PromptTotalBytes != int64(len(input)) || *rows[1].PromptTotalBytes != int64(len(full)) {
		t.Fatalf("再試行の入力計測: %+v", rows)
	}
	for _, u := range rows {
		if u.ContextReads == nil || u.ContextReads["requested"] != 1 || u.PromptBytes["initialDocuments"] == nil || *u.PromptBytes["initialDocuments"] != int64(len(marshal(doc))) || u.PromptBytes["additionalDocuments"] == nil || *u.PromptBytes["additionalDocuments"] != 0 {
			t.Fatalf("資料計測: %+v", u)
		}
	}
	if rows[0].PromptBytes["previousDraft"] != nil || rows[0].MeasurementMissingReasons["previousDraft"] != "input_unavailable" {
		t.Fatal("再開時の混在データを推定した")
	}
}

func TestMissingMeasurementsHaveFixedReasons(t *testing.T) {
	u := ai.Usage{}
	measureInput(ai.Request{Input: "未解析の本文", SchemaFile: filepath.Join(t.TempDir(), "missing")}, &u, nil)
	if u.Calls != nil || u.ContextReads != nil || u.MeasurementMissingReasons["calls"] != "provider_unavailable" || u.MeasurementMissingReasons["schema"] != "input_unavailable" || u.MeasurementMissingReasons["contextReads"] != "paths_unavailable" {
		t.Fatalf("欠測: %+v", u)
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "未解析の本文") {
		t.Fatal("本文が含まれる")
	}
}

func TestUnknownAndIncompleteInputDoesNotReportParts(t *testing.T) {
	tierPrefix := "依頼者の最初の発言だけから、出力スキーマのlevel説明に従って難易度light/standard/heavyを判定してください。理由に依頼文の該当箇所を引用してください。画像は枚数だけで内容は推測しません。\n"
	inputs := map[string]string{
		"close-review":       closePrompt + "\n" + string(marshal(Record{"messages": []any{}, "current": "秘密"})),
		"ops-watch":          incidentPrompt + "\n対象（データ）:\n[]",
		"tier":               tierPrefix + string(marshal(Record{"text": "秘密", "images": 0})),
		"incomplete-legacy":  instructions + "\n以下は相談と資料のデータです:\n" + `{"messages":[],"context":{}}`,
		"incomplete-normal":  turnInstructions + "\n以下は相談と資料のデータです:\n" + `{"messages":[],"context":{}}`,
		"incomplete-resumed": "同じ相談の続きです。固定指示と既送信資料を保ち、次の追加データと最新値だけを適用してください。\n" + `{"messages":[],"documents":`,
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			u := ai.Usage{}
			measureInput(ai.Request{Input: input, SchemaFile: filepath.Join("..", "..", "schemas", "intake.schema.json")}, &u, nil)
			if u.PromptTotalBytes == nil || *u.PromptTotalBytes != int64(len(input)) || len(u.PromptBytes) != 14 {
				t.Fatalf("合計または区分が不正: %+v", u)
			}
			for part, value := range u.PromptBytes {
				if value != nil || u.MeasurementMissingReasons[part] != "input_unavailable" {
					t.Fatalf("%s が欠測でない: %+v", part, u)
				}
			}
			b, err := json.Marshal(u)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(b, &row); err != nil {
				t.Fatal(err)
			}
			row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
			if !usage.ValidUsage(row) {
				t.Fatalf("使用量行の検査失敗: %s", b)
			}
		})
	}
}

func TestDocumentCountsDistinguishEmptyAndRepeated(t *testing.T) {
	d := Record{"project": "owner/app", "messages": []any{}, "title": "", "body": "", "questions": []any{}}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app"}}}}
	data, _, _ := m.data(false)
	u := ai.Usage{}
	measureInput(ai.Request{Input: prompt(d, data, nil, 0, nil, "")}, &u, nil)
	if u.ContextReads["requested"] != 0 || u.PromptBytes["initialDocuments"] == nil || *u.PromptBytes["initialDocuments"] != 0 {
		t.Fatalf("空の資料: %+v", u)
	}
	m.state.Projects["owner/app"].Reads = []Document{{Project: "owner/app", Path: "a.md", Text: "最初"}, {Project: "owner/app", Path: "a.md", Text: "追加"}}
	data, _, _ = m.data(false)
	measureInput(ai.Request{Input: prompt(d, data, nil, 0, nil, "")}, &u, nil)
	files := u.ContextReads["files"].([]any)
	if u.ContextReads["requested"] != 2 || u.ContextReads["repeated"] != 1 || len(files) != 1 || files[0].(map[string]any)["requested"] != 2 {
		t.Fatalf("重複資料: %+v", u)
	}
}

func TestDocumentBreakdownActualInputs(t *testing.T) {
	initial := Document{Project: "owner/app", Path: "README.md", Request: ReadRequest{File: "README.md", Mode: "full"}, Text: "資料 <初期>\n"}
	additional := Document{Project: "owner/app", Path: "README.md", Request: ReadRequest{File: "README.md", Mode: "lines", Start: 1, End: 2}, Text: "資料の追加"}
	sources := map[string]bool{documentKey(initial): true}
	d := Record{"project": "owner/app", "messages": []any{}, "title": "", "body": "秘密本文", "questions": []any{}}
	for _, tc := range []struct {
		name                        string
		docs                        []Document
		resume                      bool
		sent                        map[string]bool
		wantInitial, wantAdditional int
	}{
		{"initial", []Document{initial}, false, nil, len(marshal(initial)), 0},
		{"additional", []Document{initial, additional}, false, nil, len(marshal(initial)), len(marshal(additional))},
		{"resumed", []Document{initial, additional}, true, map[string]bool{documentKey(initial): true}, 0, len(marshal(additional))},
		{"duplicate", []Document{initial, additional, additional}, false, nil, len(marshal(initial)), 2 * len(marshal(additional))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: tc.docs}}}}
			data, shown, _ := m.data(false)
			full := prompt(d, data, nil, 0, nil, "")
			input := full
			if tc.resume {
				input = resumedPrompt(full, d, m, &sessionState{ID: "session", Sent: tc.sent}, shown, turnPromptState{})
			}
			// Source bookkeeping is kept outside both prompt formats.
			m.state.InitialDocuments = sources
			dataAfter, shownAfter, _ := m.data(false)
			fullAfter := prompt(d, dataAfter, nil, 0, nil, "")
			after := fullAfter
			if tc.resume {
				after = resumedPrompt(fullAfter, d, m, &sessionState{ID: "session", Sent: tc.sent}, shownAfter, turnPromptState{})
			}
			if input != after {
				t.Fatal("資料の分類で送信入力が変化した")
			}
			u := ai.Usage{}
			measureInput(ai.Request{Input: input}, &u, sources)
			if u.PromptBytes["initialDocuments"] == nil || *u.PromptBytes["initialDocuments"] != int64(tc.wantInitial) || u.PromptBytes["additionalDocuments"] == nil || *u.PromptBytes["additionalDocuments"] != int64(tc.wantAdditional) {
				t.Fatalf("内訳: %+v", u)
			}
			unknown := ai.Usage{}
			measureInput(ai.Request{Input: input}, &unknown, nil)
			if *unknown.PromptBytes["contextOther"] != *u.PromptBytes["contextOther"] {
				t.Fatal("分類不明でも資料がその他に残った")
			}
			b, err := json.Marshal(u)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(b, &row); err != nil {
				t.Fatal(err)
			}
			row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
			if !usage.ValidUsage(row) {
				t.Fatalf("不正な使用量: %s", b)
			}
			if strings.Contains(string(b), "秘密本文") || strings.Contains(string(b), "資料の追加") || strings.Contains(string(b), "初期") {
				t.Fatal("本文が保存される")
			}
		})
	}
}

func TestDocumentSourcesSurviveProgressReload(t *testing.T) {
	doc := Document{Project: "owner/app", Path: "README.md", Text: "本文", Request: ReadRequest{File: "README.md", Mode: "full"}}
	original := contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: []Document{doc}}}, InitialDocuments: map[string]bool{documentKey(doc): true}}
	var restored contextState
	if err := json.Unmarshal(marshal(original), &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.InitialDocuments[documentKey(restored.Projects["owner/app"].Reads[0])] {
		t.Fatal("保存後に初期資料を復元できない")
	}
	empty := contextState{InitialDocuments: map[string]bool{}}
	restored = contextState{}
	if err := json.Unmarshal(marshal(empty), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.InitialDocuments == nil || len(restored.InitialDocuments) != 0 {
		t.Fatal("空の分類を復元できない")
	}
	var legacy contextState
	if err := json.Unmarshal([]byte(`{"projects":{}}`), &legacy); err != nil || legacy.InitialDocuments != nil {
		t.Fatal("旧形式の分類不明を保持できない")
	}
}

func TestCarriedDocumentSourcesAcrossCalls(t *testing.T) {
	for _, sessionID := range []string{"", "session"} {
		t.Run("session="+sessionID, func(t *testing.T) {
			s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
				return measuredAnswer(map[string]any{"ok": true}), nil
			})
			ctx := t.Context()
			config, _ := s.options.Config(ctx)
			original, err := s.newMaterials(d, config)
			if err != nil {
				t.Fatal(err)
			}
			if err = original.bootstrap(ctx, "owner/app"); err != nil {
				t.Fatal(err)
			}
			initial := original.state.Projects["owner/app"].Reads[0]
			original.state.InitialDocuments = map[string]bool{documentKey(initial): true}
			if err = original.persist(ctx); err != nil {
				t.Fatal(err)
			}
			var carried map[string]any
			if err = json.Unmarshal(marshal(d["progress"]), &carried); err != nil {
				t.Fatal(err)
			}
			d["carriedContext"], d["progress"] = carried, nil
			m, err := s.newMaterials(d, config)
			if err != nil {
				t.Fatal(err)
			}
			session := &sessionState{ID: sessionID, Sent: map[string]bool{}}
			if err = s.restoreCarried(ctx, d, m, session); err != nil {
				t.Fatal(err)
			}
			// A later seed/read belongs to additional documents, including unread records.
			extra := Document{Project: "owner/app", Path: "missing.md", Reason: "missing", Request: ReadRequest{File: "missing.md", Mode: "full"}}
			m.state.Projects["owner/app"].Reads = append(m.state.Projects["owner/app"].Reads, extra)
			if err = m.persist(ctx); err != nil {
				t.Fatal(err)
			}
			var progress map[string]any
			if err = json.Unmarshal(marshal(d["progress"]), &progress); err != nil {
				t.Fatal(err)
			}
			d["progress"] = progress
			reloaded, err := s.newMaterials(d, config)
			if err != nil {
				t.Fatal(err)
			}
			if !reloaded.state.InitialDocuments[documentKey(initial)] {
				t.Fatal("引き継ぎ後の保存で分類が消えた")
			}
			data, shown, _ := m.data(false)
			full := prompt(d, data, nil, 0, nil, "")
			input := resumedPrompt(full, d, m, session, shown, turnPromptState{})
			var row map[string]any
			s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
				if req.Input != input {
					t.Fatal("送信入力が変わった")
				}
				b, _ := json.Marshal(u)
				_ = json.Unmarshal(b, &row)
				row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
				row["step"], row["provider"], row["model"] = req.Step, req.Provider, req.Model
				return nil
			}
			_, err = s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, filepath.Join(s.options.SchemaDir, "intake.schema.json"), input, full, nil, nil, nil, m.state.InitialDocuments)
			if err != nil {
				t.Fatal(err)
			}
			parts := row["promptBytes"].(map[string]any)
			wantExtra := len(marshal(extra))
			if sessionID != "" {
				unread := extra
				unread.Text = ""
				wantExtra = len(marshal(unread))
			}
			if parts["initialDocuments"] != float64(len(marshal(initial))) || parts["additionalDocuments"] != float64(wantExtra) || !usage.ValidUsage(row) {
				t.Fatalf("使用量: %+v", row)
			}
		})
	}
}

func TestProxyContinuationDocumentSourcesAcrossTurns(t *testing.T) {
	for _, sessionID := range []string{"", "22222222-2222-4222-8222-222222222222"} {
		t.Run("session="+sessionID, func(t *testing.T) {
			var inputs []string
			var rows []map[string]any
			modelCalls := 0
			s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
				if req.Step == usage.DraftStep {
					inputs = append(inputs, req.Input)
					modelCalls++
					if modelCalls == 1 {
						answer := reply()
						answer["readFiles"] = []any{"gitea-api", "missing.md"}
						return measuredAnswer(answer), nil
					}
				}
				return measuredAnswer(reply()), nil
			})
			ctx := t.Context()
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ consultations.Record) (consultations.Record, error) { return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
				if req.Step == usage.DraftStep {
					var row map[string]any
					if err := json.Unmarshal(marshal(u), &row); err != nil {
						return err
					}
					row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
					row["step"], row["provider"], row["model"] = req.Step, req.Provider, req.Model
					rows = append(rows, row)
				}
				return nil
			}
			cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			captured := make(chan Record, 1)
			calls := 0
			engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: cs, Turn: func(ctx context.Context, draft Record) (Record, error) {
				calls++
				if calls == 1 {
					result, err := s.Turn(ctx, draft)
					if err == nil {
						result["_continue"] = true
					}
					return result, err
				}
				captured <- draft
				<-ctx.Done()
				return nil, ctx.Err()
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = engine.Start(ctx, draftID, d, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			var next Record
			select {
			case next = <-captured:
			case <-time.After(10 * time.Second):
				engine.Close()
				t.Fatal("自動継続しない")
			}
			engine.Close()
			if len(fleet.Elements(fleet.Object(next["context"])["references"])) == 0 || len(fleet.Elements(fleet.Object(next["context"])["refused"])) == 0 {
				t.Fatalf("参照資料と読取不能資料がない: %v", next["context"])
			}
			if len(rows) == 0 {
				t.Fatal("初回使用量がない")
			}
			wantInitial := rows[0]["promptBytes"].(map[string]any)["initialDocuments"]
			if n, ok := wantInitial.(float64); !ok || n <= 0 {
				t.Fatalf("初期資料: %v", wantInitial)
			}
			// Compare the actual engine result with the old continuation (context only).
			// A new message forces a model call even if the previous answer was cached.
			next["messages"] = append(fleet.Elements(next["messages"]), Record{"role": "user", "text": "追加の回答"})
			if sessionID != "" {
				config, _ := s.options.Config(ctx)
				settings := models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}
				state := sessionState{ID: sessionID, Draft: draftID, Provider: "codex", Key: sessionKey(settings, config), Workspace: filepath.Join(s.options.RuntimeDir, "intake-workspace"), EndedAt: s.options.Now(), Messages: messageKeys(next), Sent: map[string]bool{}}
				if err := os.MkdirAll(state.Workspace, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.sessionMarker(sessionID), marshal(state), 0600); err != nil {
					t.Fatal(err)
				}
				next["aiSession"] = state
			}
			baseline := Record{}
			if err := json.Unmarshal(marshal(next), &baseline); err != nil {
				t.Fatal(err)
			}
			baseline["carriedContext"] = baseline["context"]
			run := func(draft Record) (string, map[string]any) {
				t.Helper()
				_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ consultations.Record) (consultations.Record, error) { return draft, nil })
				if err != nil {
					t.Fatal(err)
				}
				inputs, rows = nil, nil
				if _, err := s.Turn(ctx, draft); err != nil {
					t.Fatal(err)
				}
				if len(inputs) == 0 || len(rows) == 0 {
					t.Fatal("継続後の呼び出しがない")
				}
				return inputs[0], rows[0]
			}
			legacyInput, _ := run(baseline)
			actualInput, row := run(next)
			if actualInput == legacyInput {
				t.Fatal("読取方式と参照・拒否を保持した入力が要約だけの復元と同じです")
			}
			parts := row["promptBytes"].(map[string]any)
			if n, ok := parts["initialDocuments"].(float64); !ok || n <= 0 || parts["additionalDocuments"] == nil || !usage.ValidUsage(row) {
				t.Fatalf("継続後の使用量: %+v", row)
			}
		})
	}
}

func TestUnreadDocumentBreakdown(t *testing.T) {
	doc := Document{Project: "owner/app", Path: "missing.md", Reason: "missing"}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: []Document{doc}}}}}
	d := Record{"project": "owner/app", "messages": []any{}}
	data, _, _ := m.data(false)
	input := prompt(d, data, nil, 0, nil, "")
	var known, unknown ai.Usage
	measureInput(ai.Request{Input: input}, &known, map[string]bool{documentKey(doc): true})
	measureInput(ai.Request{Input: input}, &unknown, nil)
	if *known.PromptBytes["initialDocuments"] != int64(len(marshal(doc))) || *known.PromptBytes["additionalDocuments"] != 0 || unknown.MeasurementMissingReasons["initialDocuments"] != "documents_source_unknown" || *known.PromptBytes["contextOther"] != *unknown.PromptBytes["contextOther"] {
		t.Fatalf("読取不能資料: %+v / %+v", known, unknown)
	}
}

func TestContinuationPreservesInitialOutlineAndLines(t *testing.T) {
	source := "# Heading\n" + strings.Repeat("body line\n", 4000)
	var rows []ai.Usage
	s, d := testServiceSource(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil }, source)
	ctx := t.Context()
	config, err := s.options.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	m.state.InitialDocumentsCollecting = true
	if err = m.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatal(err)
	}
	if m.state.Projects["owner/app"].Reads[0].Request.Mode != "outline" {
		t.Fatal("大きな README は outline のはずです")
	}
	if _, _, err = m.read(ctx, ReadRequest{File: "owner/app:README.md", Mode: "lines", Start: 1, End: 2, Context: 5}); err != nil {
		t.Fatal(err)
	}
	var saved Record
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
		saved = copyRecord(current)
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
		if req.Step == usage.DraftStep {
			rows = append(rows, u)
		}
		return nil
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan Record, 1)
	calls := 0
	engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: cs, Turn: func(ctx context.Context, draft Record) (Record, error) {
		calls++
		if calls == 1 {
			result, err := s.Turn(ctx, draft)
			if err == nil {
				result["_continue"] = true
			}
			return result, err
		}
		captured <- draft
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Start(ctx, draftID, saved, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	var next Record
	select {
	case next = <-captured:
	case <-time.After(10 * time.Second):
		engine.Close()
		t.Fatal("自動継続しません")
	}
	engine.Close()
	if len(rows) != 1 || rows[0].PromptBytes["initialDocuments"] == nil {
		t.Fatalf("初回: %+v", rows)
	}
	want := *rows[0].PromptBytes["initialDocuments"]
	next["messages"] = append(fleet.Elements(next["messages"]), Record{"role": "user", "text": "続けてください"})
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ consultations.Record) (consultations.Record, error) { return next, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Turn(ctx, next); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 || rows[1].PromptBytes["initialDocuments"] == nil || *rows[1].PromptBytes["initialDocuments"] != want || rows[1].PromptBytes["additionalDocuments"] == nil || *rows[1].PromptBytes["additionalDocuments"] != 0 {
		t.Fatalf("継続後: want=%d rows=%+v", want, rows)
	}
}

func TestPromptBreakdownPreservesInputAndSeparatesEnvelope(t *testing.T) {
	doc := Document{Project: "owner/app", Path: "a.md", Text: "資料の秘密"}
	m := &materials{state: contextState{Notice: "付帯情報の秘密", Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Branch: "branch-secret", Reads: []Document{doc}}}}}
	ctx, shown, _ := m.data(false)
	d := Record{"project": "owner/app", "provider": "codex", "tier": "standard", "messages": []any{}, "title": "題", "body": "本文", "questions": []any{}, "references": []any{Record{"title": "参照の秘密"}}}
	input := prompt(d, ctx, nil, 2, nil, "注記の秘密")
	// 同じ入力データを再現し、予算欄だけを末尾に置いた形と照合する。
	baseline := turnInstructions + "\n以下は相談と資料のデータです:\n" + string(marshal(Record{"project": d["project"], "provider": d["provider"], "messages": d["messages"], "current": Record{"title": d["title"], "body": d["body"], "questions": d["questions"], "splitIssues": d["splitIssues"], "changeScope": d["changeScope"], "dependsOnIssues": d["dependsOnIssues"]}, "tier": d["tier"], "context": ctx, "consultationBudget": nil, "images": []Record(nil), "remainingReadRounds": 2, "references": d["references"], "notes": "注記の秘密", "controllerVerification": Record{"fixedGateOwner": "controller", "planLocation": "completionPlan.verification", "successRequiredBeforeMerge": true, "duplicateImplementerRequirement": false, "preserveExplicitAcceptanceChecks": true}}))
	baseline = strings.Replace(baseline, `"consultationBudget":null,`, "", 1)
	baseline = strings.TrimSuffix(baseline, "}") + `,"consultationBudget":null}`
	if input != baseline {
		t.Fatal("通常入力が変化した")
	}
	var u ai.Usage
	measureInput(ai.Request{Input: input, SchemaFile: filepath.Join("..", "..", "schemas", "intake.schema.json")}, &u, map[string]bool{documentKey(doc): true})
	var values map[string]json.RawMessage
	prefix := turnInstructions + "\n以下は相談と資料のデータです:\n"
	if err := json.Unmarshal([]byte(strings.TrimPrefix(input, prefix)), &values); err != nil {
		t.Fatal(err)
	}
	var contextData struct {
		Projects []struct {
			Documents      []json.RawMessage `json:"documents"`
			UnreadFiles    []json.RawMessage `json:"unreadFiles"`
			AvailableFiles json.RawMessage   `json:"availableFiles"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(values["context"], &contextData); err != nil {
		t.Fatal(err)
	}
	docBytes, fileBytes := 0, 0
	for _, p := range contextData.Projects {
		fileBytes += len(p.AvailableFiles)
		for _, raw := range append(p.Documents, p.UnreadFiles...) {
			docBytes += len(raw)
		}
	}
	want := map[string]int{"references": len(values["references"]), "notes": len(values["notes"]), "contextEnvelope": len(values["context"]) - docBytes - fileBytes}
	for part, n := range want {
		if u.PromptBytes[part] == nil || *u.PromptBytes[part] != int64(n) {
			t.Fatalf("%s: %+v", part, u)
		}
	}
	// 外側のキー・区切りと、個別分類しない固定検証メタデータを独立に数える。内部の秘密や付帯情報は残らない。
	outer := 2
	for key := range values {
		outer += len(marshal(key)) + 1
	}
	if len(values) > 0 {
		outer += len(values) - 1
	}
	outer += len(values["controllerVerification"])
	if *u.PromptBytes["contextOther"] != int64(outer) {
		t.Fatalf("外側の枠: got %d want %d", *u.PromptBytes["contextOther"], outer)
	}
	sum := int64(0)
	for part, n := range u.PromptBytes {
		if part != "schema" && n != nil {
			sum += *n
		}
	}
	if sum != *u.PromptTotalBytes {
		t.Fatalf("合計: %d != %d", sum, *u.PromptTotalBytes)
	}
	b, _ := json.Marshal(u)
	for _, secret := range []string{"資料の秘密", "付帯情報の秘密", "参照の秘密", "注記の秘密", "branch-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("本文を保存した")
		}
	}
	resume := resumedPrompt(input, d, m, &sessionState{ID: "session", Sent: map[string]bool{}}, shown, turnPromptState{})
	var resumed ai.Usage
	measureInput(ai.Request{Input: resume}, &resumed, map[string]bool{documentKey(doc): true})
	for _, part := range []string{"references", "contextEnvelope"} {
		if resumed.PromptBytes[part] != nil || resumed.MeasurementMissingReasons[part] != "input_unavailable" {
			t.Fatalf("再開時 %s: %+v", part, resumed)
		}
	}
	if resumed.PromptBytes["notes"] == nil || *resumed.PromptBytes["notes"] != int64(len(marshal(""))) {
		t.Fatal("再開で配信した注記の計測がない", resumed)
	}
	if resumed.PromptBytes["fileList"] == nil || *resumed.PromptBytes["fileList"] != 0 {
		t.Fatal("再開時の実測された空のファイル一覧")
	}
}

func TestMaximumMissingReasonsValidUsage(t *testing.T) {
	partial := true
	// partial は実際の呼出し時に設定される。
	u := ai.Usage{Partial: &partial}
	measureInput(ai.Request{Input: "未解析"}, &u, nil)
	// 各入力部位に加え、calls・contextMaxTokens・contextTotalTokens・contextReads が欠測する。
	want := len(u.PromptBytes) + 4
	if len(u.MeasurementMissingReasons) != want || want > usage.MaxMeasurementMissingReasons {
		t.Fatalf("欠測数: got %d, 入力部位 %d + 4 = %d (上限 %d): %+v", len(u.MeasurementMissingReasons), len(u.PromptBytes), want, usage.MaxMeasurementMissingReasons, u.MeasurementMissingReasons)
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
	if !usage.ValidUsage(row) {
		t.Fatalf("欠測上限の使用量行: %s", b)
	}
}

func TestNormalPromptMissingReferencesOrNotes(t *testing.T) {
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app"}}}}
	data, _, _ := m.data(false)
	d := Record{"project": "owner/app", "messages": []any{}, "references": []any{}}
	prefix := turnInstructions + "\n以下は相談と資料のデータです:\n"
	input := prompt(d, data, nil, 0, nil, "")
	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimPrefix(input, prefix)), &original); err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"references", "notes", "both"} {
		t.Run(omitted, func(t *testing.T) {
			values := make(map[string]json.RawMessage, len(original))
			for key, value := range original {
				values[key] = value
			}
			for _, key := range []string{"references", "notes"} {
				if _, ok := original[key]; !ok {
					t.Fatalf("通常入力に %s がない", key)
				}
				if omitted == key || omitted == "both" {
					delete(values, key)
				}
			}
			for _, key := range []string{"references", "notes"} {
				if _, ok := original[key]; !ok {
					t.Fatalf("複製の変更が元の %s に影響した", key)
				}
			}
			var u ai.Usage
			measureInput(ai.Request{Input: prefix + string(marshal(values))}, &u, map[string]bool{})
			for _, key := range []string{"references", "notes"} {
				if raw, ok := values[key]; ok {
					if u.PromptBytes[key] == nil || *u.PromptBytes[key] != int64(len(raw)) || u.MeasurementMissingReasons[key] != "" {
						t.Fatalf("存在する %s: %+v", key, u)
					}
				} else if u.PromptBytes[key] != nil || u.MeasurementMissingReasons[key] != "input_unavailable" {
					t.Fatalf("欠落した %s: %+v", key, u)
				}
			}
		})
	}
}
