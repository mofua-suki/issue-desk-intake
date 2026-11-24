package startup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"strings"
	"testing"
	"time"
)

func TestTransferAnswerStrictEvidenceAndUnknown(t *testing.T) {
	draft := func(text string) consultations.Record {
		return consultations.Record{"status": "draft", "messages": []any{map[string]any{"role": "assistant", "text": text}}}
	}
	for _, text := range []string{`{"relation":"unrelated","evidence":""}`, `{"relation":"unknown","evidence":"未確認"}`, "不明です", `{"relation":"required","evidence":"` + strings.Repeat("😀", 2001) + `"}`} {
		if relation, _ := transferAnswer(draft(text)); relation != "" {
			t.Fatal(text)
		}
	}
	text := "```json\n{\"relation\":\"required\",\"evidence\":\"依頼と資料を確認\"}\n```\n\n" + transferHoldNotice
	if r, e := transferAnswer(draft(text)); r != "required" || e != "依頼と資料を確認" {
		t.Fatal(r, e)
	}
	d := draft(`{"relation":"unrelated","evidence":"確認済み"}`)
	d["status"] = "thinking"
	if r, _ := transferAnswer(d); r != "" {
		t.Fatal("応答の確定前を採用")
	}
}
func TestPostgresTransferResolverReusesHeldConsultationAcrossRestart(t *testing.T) {
	db := appPostgres(t)
	ctx := t.Context()
	recordsJSON := `{"source":"owner/repo#1","report":{"acceptanceTransfers":[{"criterion":"required artifact","artifacts":["output"],"followupNumbers":[1]}]},"proposals":[{"id":"p","number":1,"status":"filed","issue":{"project":"owner/repo","number":2}}]}`
	if _, e := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view)VALUES(1,'owner/repo',1,'source','merged',jsonb_build_object('acceptanceTransferRecords',$1::jsonb)),(2,'owner/repo',2,'producer','closed','{}'),(3,'owner/repo',3,'consumer','queued','{}')`, recordsJSON); e != nil {
		t.Fatal(e)
	}
	if _, e := db.ExecContext(ctx, "INSERT INTO job_deps(job_id,depends_on)VALUES(3,1)"); e != nil {
		t.Fatal(e)
	}
	jobStore, e := jobs.New(db, jobs.Options{ReadIssue: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"number": float64(3), "body": "依頼はoutputを利用する"}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	env := pgtest.NewIsolated(t)
	started := make(chan struct{}, 1)
	open := func(turn func(context.Context, consultations.Record) (consultations.Record, error)) (*App, *store.SQLite) {
		t.Helper()
		local := openAppStore(t, env)
		records, e := consultations.OpenRecords(ctx, local.DB())
		if e != nil {
			t.Fatal(e)
		}
		service, e := consultations.New(consultations.Options{Records: records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }})
		if e != nil {
			t.Fatal(e)
		}
		engine, e := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: turn})
		if e != nil {
			t.Fatal(e)
		}
		service.BindEngine(engine)
		return &App{Config: &Config{}, Jobs: jobStore, Records: records, Consultations: service, Engine: engine}, local
	}
	first, local := open(func(ctx context.Context, _ consultations.Record) (consultations.Record, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if e = first.resolveAcceptanceTransfers(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("相談が起動されない")
	}
	checks, e := jobStore.TransferChecks(ctx)
	if e != nil || len(checks) != 1 || checks[0].ConsultationID == "" {
		t.Fatal(checks, e)
	}
	waitingJob, err := jobStore.Get(ctx, 3)
	if err != nil || fleet.Object(waitingJob.View["acceptanceTransferWait"])["blocking"] != true {
		t.Fatal("未回答で開始条件を解除", waitingJob, err)
	}
	id := checks[0].ConsultationID
	key := checks[0].Key
	first.Engine.Close()
	local.Close()
	done := make(chan struct{}, 1)
	second, local := open(func(context.Context, consultations.Record) (consultations.Record, error) {
		done <- struct{}{}
		return consultations.Record{"reply": `{"relation":"required","evidence":"依頼本文と移管資料のoutput利用を照合した"}`, "ready": true, "questions": []any{}}, nil
	})
	defer local.Close()
	defer second.Engine.Close()
	if e = second.Engine.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("再起動後に相談を再開しない")
	}
	waitTimer := time.NewTimer(2 * time.Second)
	defer waitTimer.Stop()
	for second.Engine.Busy(id) {
		select {
		case <-time.After(time.Millisecond):
		case <-waitTimer.C:
			t.Fatal("相談が終了しない")
		}
	}
	if e = second.resolveAcceptanceTransfers(ctx); e != nil {
		t.Fatal(e)
	}
	d, ok, e := second.Records.Get(ctx, "drafts", id)
	if e != nil || !ok || d["hold"] != true {
		t.Fatal(d, e)
	}
	// The intake requests this check itself, so it chooses the tier (standard).
	if d["requestedTier"] != "standard" || fleet.Object(d["tier"]) == nil || fleet.Object(d["tier"])["pending"] != nil {
		t.Fatalf("転送確認の相談の難易度: requestedTier=%v tier=%v", d["requestedTier"], d["tier"])
	}
	messages := fleet.List(d["messages"])
	users := 0
	for _, v := range messages {
		m := fleet.Object(v)
		if m["role"] == "user" {
			users++
			if m["id"] != id {
				t.Fatal(m)
			}
		}
	}
	if users != 1 {
		t.Fatal("再起動でメッセージが増えた", d)
	}
	job, e := jobStore.Get(ctx, 3)
	if e != nil {
		t.Fatal(e)
	}
	saved := fleet.Object(fleet.Object(job.View["acceptanceTransferChecks"])[key])
	if saved["relation"] != "required" || saved["consultationId"] != id {
		raw, _ := json.Marshal(d)
		t.Fatal(job.View, string(raw))
	}
	var edge int
	if e = db.QueryRowContext(ctx, "SELECT count(*)FROM job_deps WHERE job_id=3 AND depends_on=2").Scan(&edge); e != nil || edge != 1 {
		t.Fatal(edge, e)
	}
	var draftCount int
	if e = local.DB().QueryRowContext(ctx, "SELECT count(*)FROM drafts").Scan(&draftCount); e != nil || draftCount != 1 {
		t.Fatal(draftCount, e)
	}
}

type transferAIRunner struct {
	calls     atomic.Int32
	response  string
	failFirst bool
}

func (r *transferAIRunner) Call(ctx context.Context, req ai.Request) (ai.Answer, error) {
	if req.BeforeCall != nil {
		if err := req.BeforeCall(); err != nil {
			return ai.Answer{}, err
		}
	}
	n := r.calls.Add(1)

	input, output, read, write, partial := int64(10), int64(3), int64(0), int64(0), false
	questions := []any{}
	if r.response == "保留" {
		questions = []any{"資料を確認してください"}
	}
	value := map[string]any{"ready": false, "reply": r.response, "questions": questions, "readFiles": []any{}, "reviewedFiles": []any{}, "changeScope": map[string]any{"known": true, "paths": []any{}, "reason": ""}}
	if r.failFirst && n <= 2 {
		paths := []any{}
		for i := 0; i < 41; i++ {
			paths = append(paths, fmt.Sprintf("src/%d.go", i))
		}
		value["ready"], value["questions"] = false, []any{"確認"}
		value["changeScope"] = map[string]any{"known": true, "paths": paths}
	}
	return ai.Answer{Value: value, Usage: ai.Usage{InputTokens: &input, OutputTokens: &output, CacheReadTokens: &read, CacheWriteTokens: &write, Partial: &partial}}, nil
}

func TestTransferRealTurnAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name, relation string
		long, retry    bool
	}{
		{"short required", "required", false, false},
		{"truncated unrelated", "unrelated", true, false},
		{"truncated pending", "", true, false},
		{"error retry required", "required", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db := appPostgres(t)
			recordsJSON := `{"source":"owner/repo#1","report":{"acceptanceTransfers":[{"criterion":"required artifact","artifacts":["output"],"followupNumbers":[1]}]},"proposals":[{"id":"p","number":1,"status":"filed","issue":{"project":"owner/repo","number":2}}]}`
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view)VALUES(1,'owner/repo',1,'source','merged',jsonb_build_object('acceptanceTransferRecords',$1::jsonb)),(2,'owner/repo',2,'producer','closed','{}'),(3,'owner/repo',3,'consumer','queued','{}')`, recordsJSON); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on)VALUES(3,1)`); err != nil {
				t.Fatal(err)
			}
			body := "outputを利用する"
			if tc.long {
				body += strings.Repeat("x", 7000)
			}
			jobStore, err := jobs.New(db, jobs.Options{ReadIssue: func(context.Context, string, int64) (map[string]any, error) {
				return map[string]any{"number": float64(3), "body": body}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			local := newAppStore(t)
			records, err := consultations.OpenRecords(ctx, local.DB())
			if err != nil {
				t.Fatal(err)
			}
			service, err := consultations.New(consultations.Options{Records: records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			token := filepath.Join(dir, "fixture-token")
			if err = os.WriteFile(token, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token fixture" {
					t.Error("fixture authentication missing")
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/repos/owner/repo"):
					fmt.Fprint(w, `{"default_branch":"trunk"}`)
				case strings.Contains(r.URL.Path, "/branches/"):
					fmt.Fprintf(w, `{"commit":{"id":"%s"}}`, strings.Repeat("a", 40))
				case strings.Contains(r.URL.Path, "/git/trees/"):
					fmt.Fprint(w, `{"tree":[],"truncated":false}`)
				case strings.HasSuffix(r.URL.Path, "/commits"):
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected Gitea request: %s", r.URL.Path)
					http.Error(w, "missing", 404)
				}
			}))
			defer fake.Close()
			client, err := gitea.New(fake.URL, token, fake.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			response := "保留"
			evidence := "依頼と成果物を照合した"
			if tc.relation != "" {
				raw, _ := json.Marshal(map[string]string{"relation": tc.relation, "evidence": evidence})
				response = string(raw)
			}
			runner := &transferAIRunner{response: response, failFirst: tc.retry}
			config := map[string]any{"model": "fixture", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"proxy": map[string]any{"enabled": false}}}
			schema, _ := filepath.Abs(filepath.Join("..", "..", "schemas"))
			intelligence, err := consultationai.New(consultationai.Options{Config: func(context.Context) (map[string]any, error) { return config, nil }, Models: &models.Service{DB: db}, Runner: runner, Gitea: client, Records: records, RuntimeDir: dir, SchemaDir: schema})
			if err != nil {
				t.Fatal(err)
			}
			engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: intelligence.Turn})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			service.BindEngine(engine)
			app := &App{Config: &Config{}, Jobs: jobStore, Records: records, Consultations: service, Engine: engine}
			if err = app.resolveAcceptanceTransfers(ctx); err != nil {
				t.Fatal(err)
			}
			checks, err := jobStore.TransferChecks(ctx)
			if err != nil || len(checks) != 1 {
				t.Fatalf("checks=%v err=%v", checks, err)
			}
			id, key := checks[0].ConsultationID, checks[0].Key
			rawContext, err := json.Marshal(checks[0].Context)
			if err != nil {
				t.Fatal(err)
			}
			if (len([]rune(string(rawContext))) > 6500) != tc.long {
				t.Fatalf("unexpected context size=%d", len([]rune(string(rawContext))))
			}
			wait := func() consultations.Record {
				t.Helper()
				for {
					d, ok, e := records.Get(ctx, "drafts", id)
					if e != nil || !ok {
						t.Fatalf("draft=%v err=%v", d, e)
					}
					if d["status"] != "thinking" && !engine.Busy(id) {
						return d
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			d := wait()
			if tc.retry {
				if d["status"] != "error" {
					t.Fatalf("expected error, got %v", d)
				}
				if err = app.resolveAcceptanceTransfers(ctx); err != nil {
					t.Fatal(err)
				}
				if runner.calls.Load() != 2 {
					t.Fatal("error consultation restarted automatically")
				}
				if _, err = service.Chat(ctx, id, consultations.Record{"retry": true}, auth.Principal{Kind: auth.Screen}); err != nil {
					t.Fatal(err)
				}
				d = wait()
			}
			if d["status"] == "error" {
				t.Fatalf("turn failed: %v", d)
			}
			if err = app.resolveAcceptanceTransfers(ctx); err != nil {
				t.Fatal(err)
			}
			job, err := jobStore.Get(ctx, 3)
			if err != nil {
				t.Fatal(err)
			}
			saved := fleet.Object(fleet.Object(job.View["acceptanceTransferChecks"])[key])
			if fleet.Text(saved["relation"]) != tc.relation || (tc.relation != "" && saved["evidence"] != evidence) {
				t.Fatalf("saved=%v draft=%v", saved, d)
			}
			if tc.relation == "" && fleet.Object(job.View["acceptanceTransferWait"])["blocking"] != true {
				t.Fatalf("pending was not held: %v", job.View)
			}
			messages := fleet.List(d["messages"])
			if len(messages) < 2 {
				t.Fatalf("missing stored answer: %v", d)
			}
			initial := fleet.Text(fleet.Object(messages[0])["text"])
			if strings.Contains(initial, string(rawContext)) == tc.long {
				t.Fatalf("full context mismatch, truncated=%v", tc.long)
			}
			wantCalls := int32(1)
			if tc.retry {
				wantCalls = 3
			}
			if runner.calls.Load() != wantCalls {
				t.Fatalf("calls=%d draft=%v", runner.calls.Load(), d)
			}
		})
	}
}
