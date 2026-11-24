package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/references"
	"issue-desk-intake/server/internal/usage"
)

func prepareInterruptedInitialDocumentsRetry(t *testing.T, status string, input consultations.Record, sessionID string) (*Service, consultations.Record) {
	t.Helper()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	ctx := t.Context()
	config := s.options.Config
	s.options.Config = func(ctx context.Context) (map[string]any, error) {
		c, err := config(ctx)
		return map[string]any{"repositories": []any{}, "intake": c["intake"]}, err
	}
	if _, err := s.Turn(ctx, d); err == nil {
		t.Fatal("初回資料取得の失敗が必要")
	}
	s.options.Config = config
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
		current["status"] = status
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, draft consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return draft, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	next, err := cs.Chat(ctx, draftID, input, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	if next["progress"] != nil || fleet.Object(next["carriedContext"])["initialDocumentsCollecting"] != true {
		t.Fatalf("再試行の引き継ぎ: %+v", next)
	}
	if sessionID != "" {
		c, _ := config(ctx)
		settings := models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}
		state := sessionState{ID: sessionID, Draft: draftID, Provider: "codex", Key: sessionKey(settings, c), Workspace: filepath.Join(s.options.RuntimeDir, "intake-workspace"), EndedAt: s.options.Now(), Messages: messageKeys(next), Sent: map[string]bool{}}
		if err := os.MkdirAll(state.Workspace, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.sessionMarker(sessionID), marshal(state), 0600); err != nil {
			t.Fatal(err)
		}
		next["aiSession"] = state
	}
	return s, next
}

func TestInterruptedInitialDocumentsUserRetry(t *testing.T) {
	cases := []struct {
		name, status string
		input        consultations.Record
	}{
		{"retry", "error", consultations.Record{"retry": true}},
		{"new_message", "error", consultations.Record{"text": "追加の発言"}},
		{"resume_stalled", "stalled", consultations.Record{"retry": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, sessionID := range []string{"", "22222222-2222-4222-8222-222222222222"} {
				t.Run("session="+sessionID, func(t *testing.T) {
					s, next := prepareInterruptedInitialDocumentsRetry(t, tc.status, tc.input, sessionID)
					ctx := t.Context()
					var row map[string]any
					s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
						if req.Step == usage.DraftStep {
							return json.Unmarshal(marshal(u), &row)
						}
						return nil
					}
					if _, err := s.Turn(ctx, next); err != nil {
						t.Fatal(err)
					}
					var saved Record
					_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
						saved = copyRecord(current)
						return current, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					progress := fleet.Object(saved["progress"])
					if progress["initialDocumentsCollecting"] == true || len(fleet.Object(progress["initialDocuments"])) == 0 {
						t.Fatalf("再開後の集合: %+v", progress)
					}
					parts := fleet.Object(row["promptBytes"])
					if n, ok := parts["initialDocuments"].(float64); !ok || n <= 0 || parts["additionalDocuments"] == nil || fleet.Object(row["measurementMissingReasons"])["initialDocuments"] == "documents_source_unknown" {
						t.Fatalf("再試行後の使用量: %+v", row)
					}
				})
			}
		})
	}
}

func TestInterruptedInitialDocumentsResume(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	ctx := t.Context()
	config := s.options.Config
	s.options.Config = func(ctx context.Context) (map[string]any, error) {
		c, err := config(ctx)
		c = map[string]any{"repositories": []any{}, "intake": c["intake"]}
		return c, err
	}
	if _, err := s.Turn(ctx, d); err == nil {
		t.Fatal("資料取得の中断が必要")
	}
	var saved Record
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
		saved = copyRecord(current)
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	progress := fleet.Object(saved["progress"])
	if progress["initialDocumentsCollecting"] != true || progress["initialDocuments"] != nil {
		t.Fatalf("開始印: %+v", progress)
	}
	s.options.Config = config
	if _, err := s.Turn(ctx, saved); err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
		saved = copyRecord(current)
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	progress = fleet.Object(saved["progress"])
	if progress["initialDocumentsCollecting"] == true || fleet.Object(progress["initialDocuments"]) == nil || len(fleet.Object(progress["initialDocuments"])) == 0 {
		t.Fatalf("再開後の集合: %+v", progress)
	}
}

func TestMeasureInputExplicitDocumentSources(t *testing.T) {
	var measure func(ai.Request, *ai.Usage, map[string]bool) = measureInput
	doc := Document{Project: "owner/app", Path: "README.md", Request: ReadRequest{File: "README.md", Mode: "full"}, Text: "資料"}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: []Document{doc}}}}}
	data, _, _ := m.data(false)
	input := prompt(Record{"project": "owner/app", "messages": []any{}}, data, nil, 0, nil, "")
	var known, unknown ai.Usage
	measure(ai.Request{Input: input}, &known, map[string]bool{documentKey(doc): true})
	measure(ai.Request{Input: input}, &unknown, nil)
	if known.PromptBytes["initialDocuments"] == nil || *known.PromptBytes["initialDocuments"] != int64(len(marshal(doc))) || unknown.MeasurementMissingReasons["initialDocuments"] != "documents_source_unknown" {
		t.Fatalf("分類: known=%+v unknown=%+v", known, unknown)
	}
}

func TestSeedPersistsInitialDocumentsBeforeLaterFailure(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	ctx := t.Context()
	schema := s.options.SchemaDir
	s.options.SchemaDir = filepath.Join(t.TempDir(), "missing")
	if _, err := s.Turn(ctx, d); err == nil {
		t.Fatal("seed 後のスキーマ読取が失敗する必要があります")
	}
	var saved Record
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current consultations.Record) (consultations.Record, error) {
		saved = copyRecord(current)
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	progress := fleet.Object(saved["progress"])
	if progress["initialDocumentsCollecting"] == true || len(fleet.Object(progress["initialDocuments"])) == 0 {
		t.Fatalf("seed の保存: %+v", progress)
	}
	s.options.SchemaDir = schema
	var row ai.Usage
	s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
		if req.Step == usage.DraftStep {
			row = u
		}
		return nil
	}
	if _, err = s.Turn(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if row.PromptBytes["initialDocuments"] == nil || *row.PromptBytes["initialDocuments"] <= 0 || row.PromptBytes["additionalDocuments"] == nil || *row.PromptBytes["additionalDocuments"] != 0 {
		t.Fatalf("再実行の実測: %+v", row)
	}
}

// A real proxy answer can request continuation only after initial collection is persisted.
func TestProxyContinuationFinishesInitialDocumentCollection(t *testing.T) {
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		if req.Step == usage.ProxyConfirmationStep {
			return measuredAnswer(map[string]any{"answers": []any{map[string]any{
				"index": 1, "decision": "answer", "category": "fact", "answer": "実装資料です",
				"evidence": []any{map[string]any{"kind": "document", "source": "README.md", "location": "App"}},
			}}}), nil
		}
		return measuredAnswer(reply()), nil
	})
	ctx := t.Context()
	config, err := s.options.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
	result, err := s.Turn(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if result["_continue"] != true {
		t.Fatalf("代理継続が必要: %+v", result)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	progress := fleet.Object(saved["progress"])
	if progress["initialDocumentsCollecting"] == true || len(fleet.Object(progress["initialDocuments"])) == 0 {
		t.Fatalf("代理継続前の初回資料: %+v", progress)
	}
}

func TestContinuationMaterialsCacheRestoreAndLimits(t *testing.T) {
	t.Run("turn continues after list changes", func(t *testing.T) {
		rows := []references.Record{}
		for i := 0; i < 120; i++ {
			rows = append(rows, references.Record{"number": i + 1, "title": "SQLite", "body": "本文"})
		}
		draft := references.Record{"title": "SQLite"}
		initial := references.Open("owner/app", rows, nil, draft)
		match := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindStringSubmatch(initial)
		if len(match) != 2 {
			t.Fatal(initial)
		}
		calls := 0
		s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
			calls++
			v := reply()
			if calls == 1 {
				rows[0]["title"] = "変更"
				v["readFiles"] = []any{Record{"file": "open-requests", "mode": "keyword", "keyword": match[1]}}
			} else if !strings.Contains(r.Input, "一覧が変更") || !strings.Contains(r.Input, "未確認") {
				t.Errorf("理由がAI入力にない: %s", r.Input)
			}
			return measuredAnswer(v), nil
		})
		s.options.Reference = func(context.Context, Record, string) (Reference, error) {
			return Reference{Text: references.Open("owner/app", rows, nil, draft)}, nil
		}
		s.options.ReferenceContinuation = func(_ context.Context, _ Record, cursor string) (Reference, error) {
			page, err := references.OpenContinuation("owner/app", rows, nil, draft, cursor)
			return Reference{Text: page.Text}, err
		}
		result, err := s.Turn(context.Background(), d)
		if err != nil || result == nil || calls < 2 {
			t.Fatalf("turn: result=%v calls=%d err=%v", result, calls, err)
		}
		saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
		if err != nil || saved["status"] == "error" {
			t.Fatalf("saved status: %v %v", saved["status"], err)
		}
	})
	ctx := context.Background()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	config, _ := s.options.Config(ctx)
	rows := []references.Record{}
	for i := 0; i < 150; i++ {
		rows = append(rows, references.Record{"number": i + 1, "title": "SQLite", "body": "本文", "created_at": fmt.Sprintf("%03d", 150-i)})
	}
	todos := []references.Record{}
	for i := 0; i < 80; i++ {
		todos = append(todos, references.Record{"id": fmt.Sprintf("t%d", i), "body": "SQLite 本文", "createdAt": fmt.Sprintf("%03d", 80-i)})
	}
	draft := references.Record{"title": "SQLite"}
	calls := 0
	s.options.Reference = func(context.Context, Record, string) (Reference, error) {
		return Reference{Text: references.Open("owner/app", rows, todos, draft)}, nil
	}
	s.options.ReferenceContinuation = func(_ context.Context, _ Record, cursor string) (Reference, error) {
		calls++
		p, err := references.OpenContinuation("owner/app", rows, todos, draft, cursor)
		return Reference{Text: p.Text}, err
	}
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	initial, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "full", Context: 5})
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindAllStringSubmatch(initial.Text, -1)
	if len(matches) != 2 {
		t.Fatal(initial.Text)
	}
	req, err := request(map[string]any{"file": "open-requests", "mode": "keyword", "keyword": matches[0][1]})
	if err != nil {
		t.Fatal(err)
	}
	first, fresh, err := m.read(ctx, req)
	if err != nil || !fresh || !strings.Contains(first.Text, "owner/app#81") {
		t.Fatalf("first: %+v %v", first, err)
	}
	_, fresh, err = m.read(ctx, req)
	if err != nil || fresh || calls != 1 {
		t.Fatalf("cache: %v %v %d", fresh, err, calls)
	}
	next := regexp.MustCompile(`続き: ([^\n]+)`).FindStringSubmatch(first.Text)
	if len(next) != 2 {
		t.Fatal(first.Text)
	}
	req2 := req
	req2.Keyword = next[1]
	second, fresh, err := m.read(ctx, req2)
	if err != nil || !fresh || !strings.Contains(second.Text, "owner/app#111") {
		t.Fatalf("second: %+v %v", second, err)
	}
	todoReq := req
	todoReq.Keyword = matches[1][1]
	todoPage, fresh, err := m.read(ctx, todoReq)
	if err != nil || !fresh || !strings.Contains(todoPage.Text, "TODO t30") {
		t.Fatalf("todo: %+v %v", todoPage, err)
	}
	if got := projectBytes(m.state.Projects["owner/app"].Reads); got < len(initial.Text)+len(first.Text)+len(second.Text)+len(todoPage.Text) {
		t.Fatalf("bytes %d", got)
	}
	sameTurn, err := s.newMaterials(d, config)
	if err != nil || len(sameTurn.state.Projects["owner/app"].Reads) != 4 {
		t.Fatalf("same-turn progress: %+v %v", sameTurn, err)
	}
	savedDraft, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("saved draft: %v %v", found, err)
	}
	restored, err := s.newMaterials(savedDraft, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatal(err)
	}
	saved, fresh, err := restored.read(ctx, req2)
	if err != nil || fresh || saved.Text != second.Text {
		t.Fatalf("restore: %+v %v", saved, err)
	}
	continuation := s.options.ReferenceContinuation
	s.options.ReferenceContinuation = func(context.Context, Record, string) (Reference, error) {
		return Reference{}, fmt.Errorf("取得失敗")
	}
	unavailableDraft := Record{}
	for key, value := range savedDraft {
		unavailableDraft[key] = value
	}
	unavailable, err := s.newMaterials(unavailableDraft, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = unavailable.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatalf("fetch failure stopped consultation: %v", err)
	}
	if doc := unavailable.state.Projects["owner/app"].Reads[1]; !strings.Contains(doc.Reason, "取得失敗") || !strings.Contains(doc.Reason, "未確認") {
		t.Fatalf("fetch failure not visible: %+v", doc)
	}
	s.options.ReferenceContinuation = continuation
	rows[0]["title"] = "変更"
	changed, err := s.newMaterials(savedDraft, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = changed.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatalf("changed list stopped consultation: %v", err)
	}
	afterFailure, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("saved after failure: %v %v", found, err)
	}
	check, err := s.newMaterials(afterFailure, config)
	if err != nil {
		t.Fatal(err)
	}
	reads := check.state.Projects["owner/app"].Reads
	if len(reads) != 3 {
		t.Fatalf("restore discarded other requests: %+v", reads)
	}
	for _, doc := range reads {
		if !strings.Contains(doc.Reason, "未確認") || !strings.Contains(doc.Reason, "再要求") {
			t.Fatalf("missing recovery notice: %+v", doc)
		}
	}
	data, _, _ := changed.data(false)
	if encoded := string(marshal(data)); !strings.Contains(encoded, "unreadFiles") || !strings.Contains(encoded, "未確認") {
		t.Fatalf("recovery notice missing from AI input: %s", encoded)
	}
	updated, _, err := changed.read(ctx, ReadRequest{File: "open-requests", Mode: "full", Context: 5})
	if err != nil {
		t.Fatal(err)
	}
	newCursor := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindStringSubmatch(updated.Text)
	if len(newCursor) != 2 || newCursor[1] == req.Keyword {
		t.Fatalf("fresh list: %s", updated.Text)
	}
	newReq := req
	newReq.Keyword = newCursor[1]
	if doc, fresh, err := changed.read(ctx, newReq); err != nil || !fresh || doc.Reason != "" || doc.Text == "" {
		t.Fatalf("retry: %+v %v %v", doc, fresh, err)
	}
	req3 := req
	req3.Keyword = regexp.MustCompile(`続き: ([^\n]+)`).FindStringSubmatch(second.Text)[1]
	stale, fresh, err := restored.read(ctx, req3)
	if err != nil || !fresh || stale.Text != "" || !strings.Contains(stale.Reason, "一覧が変更") || !strings.Contains(stale.Reason, "未確認") {
		t.Fatalf("changed list: %+v %v %v", stale, fresh, err)
	}
	data, _, _ = restored.data(false)
	if encoded := string(marshal(data)); !strings.Contains(encoded, "一覧が変更") || !strings.Contains(encoded, "未確認") {
		t.Fatalf("changed list missing from AI input: %s", encoded)
	}
	latest, _, err := restored.read(ctx, ReadRequest{File: "open-requests", Mode: "full", Context: 5})
	if err != nil || latest.Text == initial.Text {
		t.Fatalf("stale cached list: %+v %v", latest, err)
	}
	newMatch := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindStringSubmatch(latest.Text)
	if len(newMatch) != 2 || newMatch[1] == req.Keyword {
		t.Fatalf("missing new cursor: %s", latest.Text)
	}
	newReq.Keyword = newMatch[1]
	if doc, _, e := restored.read(ctx, newReq); e != nil || doc.Text == "" || doc.Reason != "" {
		t.Fatalf("new continuation: %+v %v", doc, e)
	}
}

func TestContinuationFailureAndProjectReadLimit(t *testing.T) {
	ctx := context.Background()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	config, _ := s.options.Config(ctx)
	s.options.ReferenceContinuation = func(_ context.Context, _ Record, cursor string) (Reference, error) {
		if strings.HasSuffix(cursor, ":9") {
			return Reference{}, fmt.Errorf("取得失敗")
		}
		if strings.Contains(cursor, ":9:") {
			_, err := references.OpenContinuation("owner/app", nil, nil, nil, cursor)
			return Reference{}, err
		}
		return Reference{Text: strings.Repeat("x", 150000)}, nil
	}
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"1", "2", "3", "4"} {
		cursor := strings.Repeat("a", 64) + ":0:" + suffix
		doc, _, e := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: cursor, Context: 5})
		if e != nil || doc.Reason != "" {
			t.Fatalf("page %s: %+v %v", cursor, doc, e)
		}
	}
	doc, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: strings.Repeat("a", 64) + ":0:5", Context: 5})
	if err != nil || doc.Reason != "total" {
		t.Fatalf("limit: %+v %v", doc, err)
	}
	if _, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: strings.Repeat("a", 64) + ":0:9", Context: 5}); err == nil {
		t.Fatal("failure treated as empty")
	}
	if _, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: strings.Repeat("a", 64) + ":9:1", Context: 5}); err == nil || errors.Is(err, references.ErrOpenListChanged) {
		t.Fatalf("invalid cursor: %v", err)
	}
}

func TestOpenRequestsKeywordRestoresAsSearch(t *testing.T) {
	ctx := context.Background()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	config, _ := s.options.Config(ctx)
	s.options.Reference = func(context.Context, Record, string) (Reference, error) {
		return Reference{Text: "first\nneedle result\nlast"}, nil
	}
	s.options.ReferenceContinuation = func(context.Context, Record, string) (Reference, error) {
		t.Fatal("ordinary keyword sent as continuation")
		return Reference{}, nil
	}
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	req := ReadRequest{File: "open-requests", Mode: "keyword", Keyword: "needle", Context: 0}
	doc, _, err := m.read(ctx, req)
	if err != nil || doc.Text != "2: needle result" {
		t.Fatalf("search: %+v %v", doc, err)
	}
	saved, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("save: %v %v", found, err)
	}
	restored, err := s.newMaterials(saved, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatal(err)
	}
	doc, fresh, err := restored.read(ctx, req)
	if err != nil || fresh || doc.Text != "2: needle result" {
		t.Fatalf("restored search: %+v %v %v", doc, fresh, err)
	}
}

func TestInitialDocumentsResumeAfterFirstBlob(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("資料の復元だけでAIを呼ばない")
		return ai.Answer{}, nil
	})
	ctx := t.Context()
	config, _ := s.options.Config(ctx)
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	m.state.InitialDocumentsCollecting = true
	const project = "owner/app"
	p, err := m.project(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	guide, readme := "project guidance", "project documentation"
	p.entries = map[string]treeEntry{
		"AGENTS.md": {Path: "AGENTS.md", SHA: fixtureSHA(guide), Size: len(guide)},
		"README.md": {Path: "README.md", SHA: fixtureSHA(readme), Size: len(readme)},
	}
	readmeCalls := 0
	fixture, requests := blobFixture(t, func(r *http.Request) (string, string) {
		if strings.HasSuffix(r.URL.Path, fixtureSHA(guide)) {
			return "base64", encoded(guide)
		}
		readmeCalls++
		if readmeCalls == 1 {
			return "invalid", ""
		}
		return "base64", encoded(readme)
	})
	s.options.Gitea = fixture.options.Gitea
	if err := m.bootstrap(ctx, project); err == nil {
		t.Fatal("README.md の取得失敗が必要")
	}
	saved, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("途中まで保存された相談 found=%t err=%v", found, err)
	}

	resumed, err := s.newMaterials(saved, config)
	if err != nil {
		t.Fatal(err)
	}
	resumed.state.Projects[project].entries = p.entries
	if err := resumed.bootstrap(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := resumed.finishSeed(ctx); err != nil {
		t.Fatal(err)
	}
	docs := resumed.state.Projects[project].Reads
	if len(docs) != 2 || docs[0].Text != guide || docs[1].Text != readme || requests.Load() != 3 {
		t.Fatalf("再開後の初回資料: %+v、取得件数=%d", docs, requests.Load())
	}
	if resumed.state.InitialDocumentsCollecting || len(resumed.state.InitialDocuments) != 2 {
		t.Fatalf("収集完了の状態: %+v", resumed.state)
	}
}

func TestOpenRequestsContinuationAtProjectBudget(t *testing.T) {
	const repo = "owner/app"
	rows := make([]references.Record, 121)
	for i := range rows {
		rows[i] = references.Record{"number": i + 1, "title": "SQLite", "body": "重なり候補"}
	}
	draft := references.Record{"title": "SQLite"}
	s := &Service{options: Options{
		Reference: func(context.Context, Record, string) (Reference, error) {
			return Reference{Text: references.Open(repo, rows, nil, draft)}, nil
		},
		ReferenceContinuation: func(_ context.Context, _ Record, cursor string) (Reference, error) {
			page, err := references.OpenContinuation(repo, rows, nil, draft, cursor)
			return Reference{Text: page.Text + "続き位置は最初の資料と同じ方法で指定します。位置がなければ候補は終わりです。\n"}, err
		},
	}}
	p := &projectState{Name: repo, Branch: "trunk", Revision: strings.Repeat("a", 40), entries: map[string]treeEntry{}}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("fixtures/read-%d.md", i)
		p.Reads = append(p.Reads, Document{Path: name, Request: ReadRequest{File: name, Mode: "full"}, Text: strings.Repeat("x", maxProject/4)})
	}
	m := &materials{service: s, allowed: map[string]bool{repo: true}, draft: Record{"project": repo, "_ephemeral": true}, state: contextState{Projects: map[string]*projectState{repo: p}}}
	ctx := t.Context()
	initial, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "full"})
	cursor := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindStringSubmatch(initial.Text)
	if err != nil || initial.Reason != "" || len(cursor) != 2 {
		t.Fatalf("必須一覧の全文とカーソルが必要: reason=%q err=%v", initial.Reason, err)
	}
	doc, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: cursor[1]})
	if err != nil || doc.Reason != "" || !strings.Contains(doc.Text, "owner/app#81") {
		t.Fatalf("必須一覧の続き: %+v err=%v", doc, err)
	}
	next := regexp.MustCompile(`(?m)^続き: ([^\n]+)`).FindStringSubmatch(doc.Text)
	if len(next) != 2 {
		t.Fatal("次のカーソルが必要")
	}
	page, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: next[1]})
	if err != nil || page.Reason != "" || !strings.Contains(page.Text, "owner/app#111") {
		t.Fatalf("必須一覧の次ページ: reason=%q err=%v", page.Reason, err)
	}
	same, fresh, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: cursor[1], Start: 2, End: 3, Context: 5})
	if err != nil || fresh || same.Text != doc.Text {
		t.Fatalf("同じ続きの読み直し: fresh=%t err=%v", fresh, err)
	}
	parts := strings.Split(cursor[1], ":")
	unlistedCursor := strings.Join(parts[:2], ":") + ":81"
	if unlisted, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: unlistedCursor}); err != nil || unlisted.Reason != "total" || unlisted.Text != "" {
		t.Fatalf("一覧が示さない位置: reason=%q err=%v", unlisted.Reason, err)
	}
	optional, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "keyword", Keyword: "SQLite"})
	if err != nil || optional.Reason != "total" || optional.Text != "" {
		t.Fatalf("通常のキーワード要求: %+v err=%v", optional, err)
	}
}
