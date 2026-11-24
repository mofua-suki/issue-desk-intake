package startup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
)

type recoveryAPI struct{ calls int }

func (a *recoveryAPI) Issues(context.Context, string, string) ([]map[string]any, error) {
	a.calls++
	return nil, errors.New("unexpected issue lookup")
}

func TestRecoverAutoSubmissionTemporaryFailureThenResume(t *testing.T) {
	for _, failure := range []error{contract.NewProblem(503, "入れ替え中", "SERVER_DRAINING"), errors.New("Serverを停止中です")} {
		t.Run(failure.Error(), func(t *testing.T) {
			app := reviewNotesApp(t)
			ctx := t.Context()
			api := &recoveryAPI{}
			queue, err := jobs.New(app.DB, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			blocked := true
			app.Filing, err = filing.New(ctx, filing.Options{Records: app.Records, API: api, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }, Acquire: func(ctx context.Context, _ string) (context.Context, func(), error) {
				if blocked {
					return nil, nil, failure
				}
				return ctx, func() {}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			app.Consultations, err = consultations.New(consultations.Options{Records: app.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			events := 0
			app.Engine, err = consultations.NewEngine(ctx, consultations.EngineOptions{Service: app.Consultations, Turn: func(context.Context, consultations.Record) (consultations.Record, error) { return nil, nil }, Publish: func(consultations.Record) { events++ }})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Engine.Close()
			const id = "11111111-1111-4111-8111-111111111112"
			original := consultations.Record{"id": id, "project": "owner/app", "status": "auto_ready", "revision": 2, "ready": true, "questions": []any{}}
			if _, err = app.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, _ consultations.Record) (consultations.Record, error) { return original, nil }); err != nil {
				t.Fatal(err)
			}
			if err = app.recoverSubmissions(ctx); err != nil {
				t.Fatal(err)
			}
			saved, _, err := app.Records.Get(ctx, "drafts", id)
			if err != nil || saved["status"] != "auto_ready" || events != 0 || api.calls != 0 {
				t.Fatalf("停止中: %+v %v events=%d calls=%d", saved, err, events, api.calls)
			}
			blocked = false
			if err = app.recoverSubmissions(ctx); err != nil {
				t.Fatal(err)
			}
			if api.calls == 0 {
				t.Fatal("解除後に起票処理へ進んでいません")
			}
		})
	}
}
func (a *recoveryAPI) Issue(context.Context, string, int64) (map[string]any, error) {
	a.calls++
	return nil, errors.New("unexpected issue lookup")
}
func (a *recoveryAPI) CreateIssue(context.Context, string, any) (map[string]any, error) {
	a.calls++
	return nil, errors.New("unexpected issue creation")
}

func TestRecoverAutoSubmissionValidationFailurePostgres(t *testing.T) {
	app := reviewNotesApp(t)
	ctx := t.Context()
	api := &recoveryAPI{}
	queue, err := jobs.New(app.DB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app.Filing, err = filing.New(ctx, filing.Options{Records: app.Records, API: api, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }, SplitPlan: filing.PlanSplit})
	if err != nil {
		t.Fatal(err)
	}
	app.Consultations, err = consultations.New(consultations.Options{Records: app.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Submit: app.Filing.Submit, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	events := []consultations.Record{}
	app.Engine, err = consultations.NewEngine(ctx, consultations.EngineOptions{Service: app.Consultations, Turn: func(context.Context, consultations.Record) (consultations.Record, error) {
		t.Error("unexpected AI retry")
		return nil, nil
	}, Publish: func(d consultations.Record) { events = append(events, d) }})
	if err != nil {
		t.Fatal(err)
	}
	const id = "11111111-1111-4111-8111-111111111111"
	original := consultations.Record{"id": id, "project": "owner/app", "status": "auto_ready", "revision": 2, "ready": true, "questions": []any{}, "title": "件名", "body": "本文", "messages": []any{consultations.Record{"role": "user", "text": "相談"}}, "splitIssues": []any{consultations.Record{"title": "一件目", "purpose": "目的", "acceptance": []any{"完了"}, "dependsOn": []any{}}, consultations.Record{"title": "二件目", "purpose": "目的", "acceptance": []any{"完了"}, "dependsOn": []any{0}}}}
	_, err = app.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, _ consultations.Record) (consultations.Record, error) { return original, nil })
	if err != nil {
		t.Fatal(err)
	}
	// PostgreSQL rejects this JSON although the legacy Go decoder accepts it.
	if _, err = app.Records.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES ($1,$2)", "legacy-surrogate", `{"id":"legacy-surrogate","status":"draft","title":"\ud800"}`); err != nil {
		t.Fatal(err)
	}
	if err = app.recoverSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	saved, err := app.Consultations.Draft(ctx, id, p)
	if err != nil {
		t.Fatal(err)
	}
	failure, ok := saved["lastFailure"].(map[string]any)
	if !ok || saved["status"] != "submit_error" || !strings.Contains(fmt.Sprint(saved["error"]), "依存先") || !strings.Contains(fmt.Sprint(failure["cause"]), "依存先") || saved["body"] != "本文" || len(saved["messages"].([]any)) != 1 || len(saved["splitIssues"].([]any)) != 2 {
		t.Fatalf("復旧結果: %+v", saved)
	}
	if len(events) != 1 || fmt.Sprint(events[0]["revision"]) != fmt.Sprint(saved["revision"]) || api.calls != 0 {
		t.Fatalf("通知または送信: %+v calls=%d", events, api.calls)
	}
	actions := fmt.Sprint(app.Consultations.DraftView(saved, p, "")["actions"])
	if !strings.Contains(actions, "chat") || !strings.Contains(actions, "resubmit") {
		t.Fatalf("操作: %s", actions)
	}
	accepted, err := app.Consultations.AcceptSubmission(ctx, id, consultations.Record{"revision": saved["revision"]}, p)
	if err != nil || accepted["accepted"] != true {
		t.Fatalf("再送の受付: %v %v", accepted, err)
	}
	if err = app.recoverSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err = app.Consultations.Draft(ctx, id, p)
	if err != nil || saved["status"] != "submit_error" || !strings.Contains(fmt.Sprint(saved["error"]), "依存先") || api.calls != 0 {
		t.Fatalf("再送後: %+v %v", saved, err)
	}
	if _, err = app.Consultations.Chat(ctx, id, consultations.Record{"revision": saved["revision"], "text": "依存番号を修正してください"}, p); err != nil {
		t.Fatalf("回答を受け付けない: %v", err)
	}
}

type countedRecoveryReader struct {
	*consultations.Records
	ids  []string
	fail bool
}

func (r *countedRecoveryReader) GetDrafts(ctx context.Context, ids []string) ([]consultations.Record, error) {
	r.ids = append([]string{}, ids...)
	if r.fail {
		return nil, errors.New("full read failed")
	}
	return r.Records.GetDrafts(ctx, ids)
}
func TestRecoveryReadsOnlySelectedDrafts(t *testing.T) {
	app := reviewNotesApp(t)
	ctx := t.Context()
	for _, status := range []string{"draft", "submitting", "submit_unknown", "auto_ready", "closed"} {
		id := "selection-" + status
		_, err := app.Records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
			return consultations.Record{"id": id, "status": status, "body": "full body"}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	reader := &countedRecoveryReader{Records: app.Records}
	got, err := recoveryDrafts(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || len(reader.ids) != 3 {
		t.Fatalf("selected: %v, %v", reader.ids, got)
	}
	for _, row := range got {
		if row["status"] != "submitting" && row["status"] != "submit_unknown" && row["status"] != "auto_ready" {
			t.Fatalf("unexpected row: %v", row)
		}
	}
	reader.fail = true
	if _, err := recoveryDrafts(ctx, reader); err == nil {
		t.Fatal("full read failure lost")
	}
}

func TestRecoveryEmptyAndProjectionFailure(t *testing.T) {
	app := reviewNotesApp(t)
	ctx := t.Context()
	reader := &countedRecoveryReader{Records: app.Records}
	for _, status := range []string{"draft", "closed"} {
		id := "unselected-" + status
		if _, err := app.Records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
			return consultations.Record{"id": id, "status": status, "body": "large body"}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := recoveryDrafts(ctx, reader)
	if err != nil || len(rows) != 0 || len(reader.ids) != 0 {
		t.Fatalf("empty selection: %v %v %v", rows, reader.ids, err)
	}
	if err = app.recoverSubmissions(ctx); err != nil {
		t.Fatalf("empty recovery: %v", err)
	}
	// Invalid stored JSON forces the real PostgreSQL projection to fail; it must
	// propagate rather than being treated as an empty successful read.
	if _, err = app.Records.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES ($1,$2)", "invalid-projection", "{"); err != nil {
		t.Fatal(err)
	}
	reader.ids = nil
	if _, err = recoveryDrafts(ctx, reader); err == nil {
		t.Fatal("projection failure became success")
	}
	if reader.ids != nil {
		t.Fatalf("full read after projection failure: %v", reader.ids)
	}
	if err = app.recoverSubmissions(ctx); err == nil {
		t.Fatal("recovery swallowed projection error")
	}
}
