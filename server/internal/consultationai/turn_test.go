package consultationai

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const draftID = "11111111-1111-4111-8111-111111111111"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type callFunc func(context.Context, ai.Request) (ai.Answer, error)

func (f callFunc) Call(c context.Context, r ai.Request) (ai.Answer, error) {
	if r.BeforeCall != nil {
		if err := r.BeforeCall(); err != nil {
			return ai.Answer{}, err
		}
	}
	return f(c, r)
}
func testService(t *testing.T, call callFunc) (*Service, Record) {
	return testServiceSource(t, call, "# App\n実装資料\n")
}
func testServiceSource(t *testing.T, call callFunc, source string) (*Service, Record) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	env := pgtest.NewIsolated(t)
	db, err := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	records, err := consultations.OpenRecords(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(source), source)))
	sha := fmt.Sprintf("%x", sum)
	revision := strings.Repeat("a", 40)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fixture" {
			t.Error("認証header不足")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/owner/app"):
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		case strings.Contains(r.URL.Path, "/branches/"):
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"id": revision}})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "README.md", "type": "blob", "mode": "100644", "sha": sha, "size": len(source)}}, "truncated": false})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(source))})
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("予期しないpath %s", r.URL.Path)
			http.Error(w, "missing", 404)
		}
	})
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"repositories": []any{map[string]any{"name": "owner/app"}}, "intake": map[string]any{"proxy": map[string]any{"enabled": false}}, "assignments": map[string]any{"intakeSplit": map[string]any{"lines": 400, "files": 8}}}
	s, err := New(Options{Config: func(context.Context) (map[string]any, error) { return config, nil }, Models: &models.Service{}, Runner: call, Gitea: client, Records: records, RuntimeDir: dir, SchemaDir: func() string { p, _ := filepath.Abs(filepath.Join("..", "..", "schemas")); return p }(), Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }, Reference: func(context.Context, Record, string) (Reference, error) {
		return Reference{"fixture", "既存依頼0件を取得済み"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.options.SchemaDir, err = filepath.Abs(s.options.SchemaDir)
	if err != nil {
		t.Fatal(err)
	}
	s.resolve = func(context.Context, map[string]any, map[string]any, string) (models.Settings, error) {
		return models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}, nil
	}
	d := Record{"id": draftID, "project": "owner/app", "status": "thinking", "run": "run", "revision": 0, "tier": Record{"level": "fixed"}, "messages": []any{Record{"role": "user", "id": "u", "text": "依頼"}}}
	_, err = records.Change(ctx, "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return copyRecord(d), nil })
	if err != nil {
		t.Fatal(err)
	}
	return s, d
}
func reply() map[string]any {
	return map[string]any{"ready": false, "reply": "質問です", "questions": []any{"好みを選んでください"}, "readFiles": []any{}, "reviewedFiles": []any{"README.md"}, "estimate": Record{"lines": 100, "files": 3}}
}
func TestTurnReadsReservedReferenceBeforeReady(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		value := reply()
		value["ready"] = true
		value["title"], value["body"], value["questions"] = "題名", "実装内容", []any{}
		return measuredAnswer(value), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result["ready"] != true || fleet.Object(result["context"])["revision"] != strings.Repeat("a", 40) {
		t.Fatalf("calls=%d result=%v", calls, result)
	}
}
func TestTurnReadsReservedReferenceOnFinalRound(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if calls == 2 && !strings.Contains(r.Input, "既存依頼0件を取得済み") {
			t.Error("最終回の再回答に照合資料が渡っていない")
		}
		value := reply()
		value["ready"] = true
		value["title"], value["body"], value["questions"] = "題名", "実装内容", []any{}
		return measuredAnswer(value), nil
	})
	d["consultationBudget"] = Record{"contextRead": Record{"rounds": float64(12), "ended": true, "inputs": []any{}}}
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result["ready"] != true {
		t.Fatalf("calls=%d result=%v", calls, result)
	}
}
func TestBudgetReservationSurvivesUnknownUsageAndStops(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	config, _ := s.options.Config(context.Background())
	budget, err := s.budget(d, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.reserveCall(context.Background(), "unknown-usage-call"); err != nil {
		t.Fatal(err)
	}
	if err := budget.measured(context.Background(), ai.Request{Provider: "codex", Model: "fixture"}, ai.Usage{}); err != nil {
		t.Fatal(err)
	}
	if fleet.Number(budget.value["unknownCalls"]) != 1 {
		t.Fatal("欠測が0へ変換された")
	}
	budget.value["calls"] = 40
	out, err := s.stopBudget(context.Background(), d, budget)
	if err != nil || fleet.Object(out["consultationBudget"])["stopped"] == nil {
		t.Fatal(err, out)
	}
}
func TestReadBatchResumesWithoutNewReservation(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	config, _ := s.options.Config(context.Background())
	b, _ := s.budget(d, config)
	r, _ := b.reads()
	if err := r.begin(context.Background(), []any{"README.md"}); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := s.options.Records.Get(context.Background(), "drafts", draftID)
	b2, _ := s.budget(saved, config)
	r2, _ := b2.reads()
	m, _ := s.newMaterials(saved, config)
	if err := r2.drain(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if fleet.Number(r2.value["rounds"]) != 1 || len(fleet.Elements(fleet.Object(r2.value["active"])["pending"])) != 0 {
		t.Fatal(r2.value)
	}
}

func TestRejectedProjectReadRecovery(t *testing.T) {
	for _, failure := range []string{"connection", "authentication", "temporary"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
				calls++
				if !strings.Contains(req.Input, "保存された資料要求を取り除きました") || !strings.Contains(req.Input, "start") {
					t.Error("訂正案内が次の回答に届いていません")
				}
				return measuredAnswer(reply()), nil
			})
			config, _ := s.options.Config(ctx)
			b, _ := s.budget(d, config)
			r, _ := b.reads()
			r.value["rounds"] = float64(10)
			if err := r.begin(ctx, []any{"outside/repo:secret.md", "README.md"}); err != nil {
				t.Fatal(err)
			}
			saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			original := s.options.Gitea
			forbidden, allowed := 0, 0
			client, err := gitea.New("https://gitea.invalid", filepath.Join(s.options.RuntimeDir, "token"), transportFunc(func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "outside/repo") {
					forbidden++
				} else {
					allowed++
				}
				if failure == "connection" {
					return nil, errors.New("connection failed")
				}
				w := httptest.NewRecorder()
				if failure == "authentication" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				} else {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				}
				return w.Result(), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			s.options.Gitea = client
			b, _ = s.budget(saved, config)
			r, _ = b.reads()
			m, _ := s.newMaterials(saved, config)
			if err := r.drain(ctx, m); err == nil {
				t.Fatal("取得障害がエラーになりません")
			}
			saved, _, err = s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			state := fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])
			pending := fleet.Elements(fleet.Object(state["active"])["pending"])
			if forbidden != 0 || allowed == 0 || len(pending) != 1 || pending[0] != "README.md" || fleet.Number(state["rounds"]) != 11 || !strings.Contains(fleet.Text(state["invalidReadNotice"]), "再要求") {
				t.Fatalf("forbidden=%d allowed=%d state=%v", forbidden, allowed, state)
			}
			s.options.Gitea = original
			result, err := s.Turn(ctx, saved)
			if err != nil {
				t.Fatal(err)
			}
			if calls == 0 || fleet.Text(result["reply"]) == "" {
				t.Fatalf("calls=%d result=%v", calls, result)
			}
			saved, _, err = s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			state = fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])
			if len(fleet.Elements(fleet.Object(state["active"])["pending"])) != 0 || fleet.Number(state["rounds"]) != 11 {
				t.Fatalf("state=%v", state)
			}
			progress := fleet.Object(saved["progress"])
			projects := fleet.Object(progress["projects"])
			if len(fleet.Elements(fleet.Object(projects["owner/app"])["reads"])) == 0 {
				t.Fatalf("資料が保存されていません: %v", progress)
			}
		})
	}
}
func TestReservedReferenceDoesNotProveRepositoryBody(t *testing.T) {
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Name: "owner/app", Reads: []Document{{Project: "owner/app", Path: "docs/intake-skill-details.md", Request: ReadRequest{Mode: "full"}, Text: "文"}}}}}}
	_, _, bodies := m.data(false)
	if len(bodies) != 0 {
		t.Fatal("予約資料をrepo本文と扱った")
	}
}
func TestProxyRefusesPermissionEvenWhenAIAnswers(t *testing.T) {
	answers, reasons, err := proxyVerdicts(Record{"answers": []any{Record{"index": 1, "decision": "answer", "category": "permission", "answer": "許可します", "evidence": []any{Record{"kind": "recommendation", "source": "AI"}}}}}, []any{"承認するか"}, nil)
	if err != nil || len(answers) != 0 || len(reasons) != 1 {
		t.Fatal(err, answers, reasons)
	}
}
func TestRepairLimitPersistsAcrossTurns(t *testing.T) {
	s, d := testService(t, nil)
	config, _ := s.options.Config(context.Background())
	for i := 0; i < 8; i++ {
		ok, err := s.repair(context.Background(), d, config, map[string]int{}, "draft_format")
		if err != nil || !ok {
			t.Fatal(i, ok, err)
		}
	}
	ok, err := s.repair(context.Background(), d, config, map[string]int{}, "reviewed_unconfirmed")
	if err != nil || ok {
		t.Fatal(ok, err)
	}
}

func TestTurnLogsZeroBasedDependenciesAndPromptsOneBased(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if !strings.Contains(r.Input, "分割案のdependsOnは表示順の1始まり") {
			t.Error("固定指示に依存番号の説明がありません")
		}
		raw, err := os.ReadFile(r.SchemaFile)
		if err != nil || !strings.Contains(string(raw), "表示順に1、2、3") {
			t.Fatal("配備スキーマに依存番号の説明がありません", err)
		}
		value := reply()
		value["splitIssues"] = []any{map[string]any{"dependsOn": []any{}}, map[string]any{"dependsOn": []any{float64(0), float64(1)}}}
		return measuredAnswer(value), nil
	})
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("受付AIが呼ばれていません")
	}
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "相談AIの分割案の依存番号" {
			if entry["id"] != draftID || entry["plans"] != float64(2) || entry["references"] != float64(2) || entry["zero"] != float64(1) {
				t.Fatalf("ログが不正です: %v", entry)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("依存番号のログがありません: %s", logs.String())
	}
}

func TestTurnWithoutSplitDoesNotLogDependencies(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		return measuredAnswer(reply()), nil
	})
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "相談AIの分割案の依存番号") {
		t.Fatalf("分割案のないログ: %s", logs.String())
	}
}

func TestDependencyLogCountsOnlyValidNumericZeros(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	logSplitDependencies("test", Record{"splitIssues": []any{Record{"dependsOn": []any{json.Number("0"), float64(0), int(0), int64(0), json.Number("invalid"), "0", nil, json.Number("1"), 0.5}}}})
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["references"] != float64(9) || entry["zero"] != float64(4) {
		t.Fatalf("unexpected log: %v", entry)
	}
}

func TestNewProjectHoldConditions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project any
		held    bool
	}{
		{"needed", Record{"needed": true}, true}, {"not needed", Record{"needed": false}, false}, {"missing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Record{"ready": true, "reply": "元の回答", "newProject": tc.project}
			holdNewProject(a)
			holdNewProject(a)
			if (a["ready"] == false) != tc.held {
				t.Fatalf("ready: %v", a)
			}
			count := strings.Count(fleet.Text(a["reply"]), "チャットのAIに相談")
			if (count == 1) != tc.held {
				t.Fatalf("reply: %v", a)
			}
		})
	}
	a := endAnswer(Record{}, Record{"ready": true, "newProject": Record{"needed": true}})
	if a["ready"] != false || !strings.Contains(fleet.Text(a["reply"]), "チャットのAIに相談") {
		t.Fatal(a)
	}
}

func TestOpsWatchResendRestrictions(t *testing.T) {
	var input string
	s, _ := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		input = r.Input
		return measuredAnswer(map[string]any{}), nil
	})
	if _, err := s.Answer(t.Context(), map[string]any{"kind": "ops-watch", "schema": `{"type":"object"}`}); err != nil {
		t.Fatal(err)
	}
	for _, restriction := range []string{
		"有事では同じ有事の同じ行動は1回まで",
		"job_recovery では同じ原因の送信は1回まで、異なる原因は別に判断します",
		"同じ原因の修復送信も1回まで",
	} {
		if !strings.Contains(input, restriction) {
			t.Errorf("再送制限がAIへの入力にありません: %s", restriction)
		}
	}
}

func TestEndAnswerNewProjectNoticeOrder(t *testing.T) {
	answer := Record{"ready": true, "reply": "元の回答", "newProject": Record{"needed": true}, "readFiles": []any{"README.md"}}
	// Turn also applies the hold before it reaches endAnswer.
	holdNewProject(answer)
	answer = endAnswer(Record{}, answer)
	want := "元の回答\n新しいリポジトリが必要な場合は、チャットのAIに相談してください。\n追加資料の確認枠を終了しました。確認できない事項は保存済みの下書きに残し、同じ要求を繰り返しません。"
	if fleet.Text(answer["reply"]) != want || answer["ready"] != false || len(fleet.Elements(answer["readFiles"])) != 0 {
		t.Fatalf("終了時の回答: %#v", answer)
	}
	if fixture := os.Getenv("ISSUE_DESK_BROWSER_ANSWER_FIXTURE"); fixture != "" {
		data, err := json.Marshal(fleet.Text(answer["reply"]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTurnStopsNewProjectAndRestoredAnswer(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprintf("restored=%v", restored), func(t *testing.T) {
			calls := 0
			old := Record(reply())
			old["ready"] = true
			old["newProject"] = Record{"needed": true, "summary": "新規", "reason": "新規リポジトリ"}
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(old), nil })
			if restored {
				config, _ := s.options.Config(context.Background())
				b, err := s.budget(d, config)
				if err != nil {
					t.Fatal(err)
				}
				reads, err := b.reads()
				if err != nil {
					t.Fatal(err)
				}
				if err := reads.remember(context.Background(), old); err != nil {
					t.Fatal(err)
				}
				d, _, err = s.options.Records.Get(context.Background(), "drafts", draftID)
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if result["ready"] != false || result["status"] == "project_handoff" || result["projectHandoff"] != nil || !strings.Contains(fleet.Text(result["reply"]), "チャットのAIに相談") {
				t.Fatalf("result=%v", result)
			}
			if restored && calls != 0 || !restored && calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
			if err != nil || saved["projectHandoff"] != nil {
				t.Fatalf("saved=%v err=%v", saved, err)
			}
			projects, err := s.options.Records.List(context.Background(), "projects")
			if err != nil || len(projects) != 0 {
				t.Fatalf("projects=%v err=%v", projects, err)
			}
		})
	}
}

func TestNewProjectHoldSkipsProxyAndKeepsSession(t *testing.T) {
	s, d := testService(t, nil)
	t.Setenv("CODEX_HOME", t.TempDir())
	answer := Record{"ready": false, "questions": []any{"質問"}, "newProject": Record{"needed": true}}
	got, err := s.proxy(context.Background(), d, answer, map[string]any{}, nil, models.Settings{}, false)
	if err != nil || got["_continue"] != nil {
		t.Fatalf("proxy: %v %v", got, err)
	}
	state := &sessionState{Draft: draftID, Provider: "codex", Workspace: filepath.Join(s.options.RuntimeDir, "intake-workspace")}
	id := "22222222-2222-4222-8222-222222222222"
	if err := s.rememberSession(context.Background(), d, state, id); err != nil {
		t.Fatal(err)
	}
	if err := s.finishSession(context.Background(), d, state, answer); err != nil || state.ID != id || state.EndedAt.IsZero() {
		t.Fatalf("held session: %v %v", state, err)
	}
	// 確定できる回答の後も、了承・訂正の返信を同じ会話へ続けるため残す。
	if err := s.finishSession(context.Background(), d, state, Record{"ready": true}); err != nil || state.ID != id {
		t.Fatalf("ready session: %v %v", state, err)
	}
	if err := s.finishSession(context.Background(), d, state, Record{"escalate": true}); err != nil || state.ID != "" {
		t.Fatalf("finished session: %v %v", state, err)
	}
}

func TestTurnGivesOpenRequestsBeforeFirstCall(t *testing.T) {
	var input string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		input = r.Input
		return measuredAnswer(reply()), nil
	})
	s.options.EagerOpenRequests = true
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, "既存依頼0件を取得済み") {
		t.Fatalf("一覧が初回の入力にありません: %s", input)
	}
	if rounds := fleet.Number(fleet.Object(result["progress"])["rounds"]); rounds != 0 {
		t.Fatalf("読み足しの回数に数えています: %v", rounds)
	}
}

func TestTurnContinuesWhenOpenRequestsUnavailable(t *testing.T) {
	var input string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		input = r.Input
		return measuredAnswer(reply()), nil
	})
	s.options.EagerOpenRequests = true
	s.options.Reference = func(context.Context, Record, string) (Reference, error) {
		return Reference{}, errors.New("一覧の取得に失敗")
	}
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, openRequestsUnavailable) {
		t.Fatalf("未確認の知らせがありません: %s", input)
	}
}

func TestCompactKeepsOpenRequestsBody(t *testing.T) {
	m := &materials{state: contextState{Projects: map[string]*projectState{"p/r": {Name: "p/r", Reads: []Document{
		{Path: "open-requests", Text: "一覧の本文", Request: ReadRequest{File: "open-requests", Mode: "full"}},
		{Path: "a.go", Text: "古い本文", Request: ReadRequest{File: "a.go", Mode: "full"}},
		{Path: "b.go", Text: "最後の本文", Request: ReadRequest{File: "b.go", Mode: "full"}},
	}}}}, draft: Record{}}
	data, _, _ := m.data(true)
	encoded := string(marshal(data))
	if !strings.Contains(encoded, "一覧の本文") || strings.Contains(encoded, "古い本文") {
		t.Fatal(encoded)
	}
}

func TestIdentityIgnoresOpenRequestsVersion(t *testing.T) {
	build := func(version string) *materials {
		return &materials{state: contextState{Projects: map[string]*projectState{"p/r": {Name: "p/r", Reads: []Document{{Path: "open-requests", Version: version, SHA256: version}}}}}}
	}
	if build("a").identity() != build("b").identity() {
		t.Fatal("一覧の版が変わると会話を捨ててしまいます")
	}
}

func TestTurnReadsFullOpenRequestsAfterPartialRead(t *testing.T) {
	const remainingCandidate = "2件目の重なり候補"
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		answer := reply()
		switch calls {
		case 1:
			answer["questions"] = []any{}
			answer["readFiles"] = []any{Record{"file": "open-requests", "mode": "lines", "start": 1, "end": 1}}
		case 2, 3:
			if hasCandidate := strings.Contains(r.Input, remainingCandidate); hasCandidate != (calls == 3) {
				t.Fatalf("依頼一覧の全文が渡る段が違います: call=%d full=%t", calls, hasCandidate)
			}
			if calls == 3 && strings.Contains(r.Input, openRequestsUnavailable) {
				t.Fatal("取得済みの一覧に未取得の通知が残りました")
			}
			answer["ready"] = true
			answer["title"], answer["body"], answer["questions"] = "題名", "実装内容", []any{}
		default:
			t.Fatalf("想定しない再回答: %d", calls)
		}
		return measuredAnswer(answer), nil
	})
	s.options.EagerOpenRequests = true
	reads := 0
	s.options.Reference = func(context.Context, Record, string) (Reference, error) {
		reads++
		if reads == 1 {
			return Reference{}, errors.New("起動時の一覧取得だけ失敗")
		}
		return Reference{Text: "1件目の候補\n" + remainingCandidate}, nil
	}
	answer, err := s.Turn(t.Context(), d)
	if err != nil || answer["ready"] != true || calls != 3 || reads != 3 {
		t.Fatalf("ready=%v AI呼出し=%d 一覧取得=%d err=%v", answer["ready"], calls, reads, err)
	}
}

func TestTurnDoesNotReusePendingAnswerBeforeFullListWasDelivered(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if !strings.Contains(r.Input, "既存依頼0件を取得済み") {
			t.Fatal("再回答に既存依頼の一覧がありません")
		}
		answer := reply()
		answer["ready"] = true
		answer["title"], answer["body"], answer["questions"] = "再照合した題名", "実装内容", []any{}
		return measuredAnswer(answer), nil
	})
	ctx := t.Context()
	config, _ := s.options.Config(ctx)
	budget, err := s.budget(d, config)
	if err != nil {
		t.Fatal(err)
	}
	reads, err := budget.reads()
	if err != nil {
		t.Fatal(err)
	}
	old := reply()
	old["ready"] = true
	old["title"], old["body"], old["questions"] = "古い題名", "実装内容", []any{}
	if err := reads.remember(ctx, old); err != nil {
		t.Fatal(err)
	}
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatal(err)
	}
	// The list can return to the same version as an earlier answer.
	// That earlier delivery does not identify what the pending answer saw.
	doc, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "full", Context: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.rememberDelivered(ctx, map[string]bool{documentKey(doc): true}); err != nil {
		t.Fatal(err)
	}
	previous := doc
	previous.SHA256 = digest("別の候補一覧")
	reads.value["answerOpenRequestsKey"] = documentKey(previous)
	if err := reads.save(ctx); err != nil {
		t.Fatal(err)
	}
	saved, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("保存した相談 found=%t err=%v", found, err)
	}
	answer, err := s.Turn(ctx, saved)
	if err != nil || answer["ready"] != true || answer["title"] != "再照合した題名" || calls != 1 {
		t.Fatalf("再開 answer=%v calls=%d err=%v", answer, calls, err)
	}
}
