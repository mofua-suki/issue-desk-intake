package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/store"
)

const stopID = "44444444-4444-4444-8444-444444444444"

func stopInput(d Record) Record {
	return Record{"operationId": stopID, "revision": d["revision"], "reason": "使用量の異常を確認したため保留"}
}
func waitStopEngine(t *testing.T, e *Engine) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for e.Busy(todoID) && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if e.Busy(todoID) {
		t.Fatal("取消後の相談が終了しません")
	}
}
func stopHTTP(t *testing.T, s *Service, token string, input Record) (int, Record) {
	t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+todoID+"/stop", strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	s.Register(mux)
	mux.ServeHTTP(w, r)
	var out Record
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(w.Body.String(), err)
	}
	return w.Code, out
}
func stopToken(t *testing.T, db *store.SQLite, delegated bool) string {
	t.Helper()
	token := "idk_" + strings.Repeat("a", 43)
	if _, err := db.AddPCToken(context.Background(), store.PCTokenRecord{ID: "monitor", Name: "fixture", Hash: auth.Hash(token), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetDelegated(context.Background(), "monitor", delegated, time.Now()); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestStopHTTPPersistsFenceBeforeCancellationAndSurvivesRecovery(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(run context.Context, d Record) (Record, error) {
		close(started)
		<-run.Done()
		close(cancelled)
		<-release // Even a late successful answer cannot overwrite the stop fence.
		return Record{"ready": true, "reply": "遅延した回答", "body": "上書き禁止"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		engine.Close()
	}()
	s.BindEngine(engine)
	p := auth.Principal{Kind: auth.Screen}
	if _, err = s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "hold": true}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "保存する依頼原文"}, p); err != nil {
		t.Fatal(err)
	}
	<-started
	before, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["error"], d["errorCode"] = "古いエラー", "old"
		d["consultationBudget"] = Record{"calls": 2, "weighted": 1200, "unknownCalls": 0}
		d["body"], d["questions"], d["future"] = "保存する本文", []any{"質問"}, Record{"keep": true}
		// SplitChildren checkpoints the child IDs while the parent still thinks.
		d["consultationChildren"] = []any{photoID}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	child := Record{"id": photoID, "project": "owner/app", "status": "thinking", "parentConsultation": todoID, "revision": 0, "messages": []any{"子の原文"}}
	if _, err = s.options.Records.Change(ctx, "drafts", photoID, func(*sql.Tx, Record) (Record, error) { return child, nil }); err != nil {
		t.Fatal(err)
	}
	token := stopToken(t, db, true)
	input := stopInput(before)
	code, out := stopHTTP(t, s, token, input)
	if code != 200 || out["status"] != "stalled" || out["stallCause"] != "explicit" {
		t.Fatal(code, out)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("実行ctxが取り消されません")
	}
	stopped, _ := s.Draft(ctx, todoID, p)
	for _, key := range []string{"body", "messages", "questions", "consultationBudget", "future", "hold"} {
		a, _ := canonicalJSON(before[key])
		b, _ := canonicalJSON(stopped[key])
		if a != b {
			t.Fatal("保存済み内容が変化", key)
		}
	}
	if stopped["errorCode"] != nil || stopped["run"] != nil || stopped["retryAt"] != nil || stopped["queued"] != false || len(fleet.Items(stopped["consultationStops"])) != 1 {
		t.Fatal(stopped)
	}
	savedChild, _, err := s.options.Records.Get(ctx, "drafts", photoID)
	aChild, _ := canonicalJSON(child)
	bChild, _ := canonicalJSON(savedChild)
	if err != nil || aChild != bChild {
		t.Fatal("別IDの子相談を暗黙に変更しました", err)
	}
	code, out = stopHTTP(t, s, token, input)
	if code != 200 || len(fleet.Items(out["consultationStops"])) != 1 {
		t.Fatal("停止の再送が非冪等", code, out)
	}
	conflict := clone(input)
	conflict["reason"] = "別内容"
	if code, _ = stopHTTP(t, s, token, conflict); code != 409 {
		t.Fatal("操作IDの別内容を許可", code)
	}
	close(release)
	waitStopEngine(t, engine)
	after, _ := s.Draft(ctx, todoID, p)
	a, _ := canonicalJSON(stopped)
	b, _ := canonicalJSON(after)
	if a != b {
		t.Fatal("遅延回答が停止記録を上書き")
	}
	next, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(_ context.Context, d Record) (Record, error) {
		if d["id"] == todoID {
			t.Error("停止済み相談を再起動で再開")
		}
		return Record{"ready": false}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	s.BindEngine(next)
	if err = next.Recover(ctx); err != nil || next.Busy(todoID) {
		t.Fatal(err)
	}
}

func TestStallCauseClearedOnResume(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	started := make(chan context.Context, 2)
	release := make(chan struct{})
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(run context.Context, d Record) (Record, error) {
		started <- run
		select {
		case <-run.Done():
			return nil, run.Err()
		case <-release:
			return Record{"ready": false, "reply": "確認"}, nil
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		engine.Close()
	}()
	s.BindEngine(engine)
	p := auth.Principal{Kind: auth.Screen}
	s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "hold": true}, p, false)
	if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	firstContext := <-started
	d, _ := s.Draft(ctx, todoID, p)
	input := stopInput(d)
	if _, err = db.DB().Exec(`CREATE FUNCTION stop_save_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture save failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB().Exec(`CREATE TRIGGER stop_save_failure BEFORE UPDATE ON drafts FOR EACH ROW WHEN ((NEW.json::jsonb ->> 'consultationStops') IS NOT NULL) EXECUTE FUNCTION stop_save_failure()`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Stop(ctx, todoID, input, p); err == nil {
		t.Fatal("保存失敗を成功にしました")
	}
	d, _ = s.Draft(ctx, todoID, p)
	if d["status"] != "thinking" || d["stallCause"] != nil || !engine.Busy(todoID) || firstContext.Err() != nil {
		t.Fatal("保存前に取り消しました", d)
	}
	if _, err = db.DB().Exec(`DROP TRIGGER stop_save_failure ON drafts`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Stop(ctx, todoID, input, p); err != nil {
		t.Fatal(err)
	}
	waitStopEngine(t, engine)
	d, _ = s.Draft(ctx, todoID, p)
	beforeResume, _ := canonicalJSON(d)
	if _, err = s.Chat(ctx, todoID, Record{"retry": true, "expectedRevision": -1}, p); err == nil {
		t.Fatal("不正な再開を許可")
	}
	if _, err = db.DB().Exec(`CREATE TRIGGER stop_save_failure BEFORE UPDATE ON drafts FOR EACH ROW EXECUTE FUNCTION stop_save_failure()`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"retry": true, "revision": d["revision"]}, p); err == nil {
		t.Fatal("再開の保存失敗を成功にしました")
	}
	if _, err = db.DB().Exec(`DROP TRIGGER stop_save_failure ON drafts`); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := s.Draft(ctx, todoID, p)
	afterResumeFailure, _ := canonicalJSON(unchanged)
	if beforeResume != afterResumeFailure || unchanged["stallCause"] != "explicit" {
		t.Fatal("拒否または保存失敗で停止状態が変化", unchanged)
	}
	if _, err = s.Chat(ctx, todoID, Record{"retry": true, "revision": d["revision"]}, p); err != nil {
		t.Fatal("明示再試行できません", err)
	}
	<-started
	if replay, err := s.Stop(ctx, todoID, input, p); err != nil || replay["status"] != "thinking" || replay["stallCause"] != nil || len(fleet.Items(replay["consultationStops"])) != 1 {
		t.Fatal("旧停止の再送が新実行を変更", replay, err)
	}
	close(release)
	waitStopEngine(t, engine)
	d, _ = s.Draft(ctx, todoID, p)
	if d["status"] != "draft" || len(fleet.Items(d["messages"])) != 2 {
		t.Fatal("旧操作が新実行を取り消しました", d)
	}
	// A subsequent run can expire while the prior explicit-stop history remains.
	d, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, current Record) (Record, error) {
		current["status"] = "thinking"
		current["waitStartedAt"] = timestamp(s.options.Now().Add(-25 * time.Hour))
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Start(ctx, todoID, d, p); err != nil {
		t.Fatal(err)
	}
	waitStopEngine(t, engine)
	d, err = s.Draft(ctx, todoID, p)
	if err != nil || d["status"] != "stalled" || d["stallCause"] != "timeout" || len(fleet.Items(d["consultationStops"])) != 1 {
		t.Fatal(d, err)
	}
}

func TestStopRejectsUndelegatedAndPublishedOrClosedWithoutMutation(t *testing.T) {
	for _, status := range []string{"closed", "submitted", "submitting", "submit_unknown", "auto_ready", "project_handoff"} {
		t.Run(status, func(t *testing.T) {
			s, db, _ := fixture(t)
			e, _ := NewEngine(context.Background(), EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, nil }})
			defer e.Close()
			s.BindEngine(e)
			d := Record{"id": todoID, "project": "owner/app", "status": status, "revision": 3, "body": "公開原文"}
			if _, err := s.options.Records.Change(context.Background(), "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
			token := stopToken(t, db, false)
			if code, _ := stopHTTP(t, s, token, stopInput(d)); code != 403 {
				t.Fatal("非委任PCの停止を許可", code)
			}
			db.SetDelegated(context.Background(), "monitor", true, time.Now())
			if code, _ := stopHTTP(t, s, token, stopInput(d)); code != 409 {
				t.Fatal("公開済みまたは終了済みを停止", code)
			}
			after, _ := s.Draft(context.Background(), todoID, auth.Principal{Kind: auth.Screen})
			a, _ := canonicalJSON(d)
			b, _ := canonicalJSON(after)
			if a != b {
				t.Fatal("拒否で原文を変更")
			}
		})
	}
}

func TestStopDoesNotHoldBusyMutexWhileWaitingForWriter(t *testing.T) {
	s, _, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s.BindEngine(e)
	d := Record{"id": todoID, "project": "owner/app", "status": "thinking", "revision": 0}
	if _, err = s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	entered, inspectBusy := make(chan struct{}), make(chan struct{})
	writerDone, stopped := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.options.Records.Change(ctx, "drafts", todoID, func(tx *sql.Tx, current Record) (Record, error) {
			// Hold the PostgreSQL row lock before starting Stop, so the writer
			// remains first even when Stop reaches its update immediately.
			var locked string
			if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=? FOR UPDATE"), todoID).Scan(&locked); err != nil {
				close(entered)
				return nil, err
			}
			close(entered)
			select {
			case <-inspectBusy:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			e.Busy(todoID) // Chat/Finish consult Busy inside their write transaction.
			return current, nil
		})
		writerDone <- err
	}()
	<-entered
	go func() { _, err := s.Stop(ctx, todoID, stopInput(d), auth.Principal{Kind: auth.Screen}); stopped <- err }()
	for e.controlMu.TryLock() {
		e.controlMu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("停止の直列化に到達しません")
		case <-time.After(time.Millisecond):
		}
	}
	close(inspectBusy)
	for _, done := range []chan error{writerDone, stopped} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("保存と実行状態の検査が相互待ちになりました")
		}
	}
}
