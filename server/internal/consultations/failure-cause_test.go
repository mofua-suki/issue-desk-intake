package consultations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

const (
	oldFixedFailure   = "相談の回答を確認できませんでした。保存済みの相談から処理を確認してください。"
	failureBody       = "利用者だけが知っている相談の本文です"
	failureSecretPart = "idk_fixtureSecretValue"
)

// failedDraft は失敗する Turn を本番と同じ Engine で動かし、保存された相談とログを返す。
func failedDraft(t *testing.T, turnErr error, report bool) (Record, []string) {
	t.Helper()
	s, _, _ := fixture(t)
	ctx := context.Background()
	screen := auth.Principal{Kind: auth.Screen}
	if _, err := s.acceptReceipt(ctx, Record{"id": todoID, "project": "owner/app", "text": failureBody, "hold": true, "tier": "light"}, "issue", todoID, todoID, screen); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	lines := []string{}
	options := EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, turnErr }}
	if report {
		options.Report = func(err error) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, err.Error())
		}
	}
	engine, err := NewEngine(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	s.options.Start = engine.Start
	if err := engine.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d, err := s.Draft(ctx, todoID, screen)
		if err == nil && (text(d["status"]) == "error" || record(d["lastFailure"]) != nil) {
			for engine.Busy(todoID) && text(d["status"]) == "error" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			return d, append([]string{}, lines...)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("失敗が保存されませんでした")
	return nil, nil
}

func TestTurnFailureKeepsStageAndCauseWithoutSecretsOrConversation(t *testing.T) {
	raw := fmt.Errorf("open /state/x.json: permission denied token=%s 入力「%s」", failureSecretPart, failureBody)
	d, lines := failedDraft(t, AtStage("資料の読取", raw), true)
	failure := record(d["lastFailure"])
	if failure["stage"] != "資料の読取" || !strings.Contains(text(failure["cause"]), "permission denied") {
		t.Fatalf("段階と原因が記録にありません: %+v", failure)
	}
	if text(d["error"]) == oldFixedFailure || !strings.Contains(text(d["error"]), "permission denied") || !strings.Contains(text(d["error"]), "資料の読取") {
		t.Fatalf("固定文だけに置き換わっています: %q", d["error"])
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "permission denied") || !strings.Contains(lines[0], "stage=資料の読取") {
		t.Fatalf("ログに原因がありません: %q", lines)
	}
	for _, target := range append([]string{text(d["error"]), text(failure["cause"]), text(failure["message"])}, lines...) {
		if strings.Contains(target, failureSecretPart) || strings.Contains(target, failureBody) {
			t.Fatalf("資格情報か本文が残っています: %q", target)
		}
	}
}

func TestDraftViewShowsLastFailureToReader(t *testing.T) {
	s, _, _ := fixture(t)
	d := Record{"id": todoID, "status": "error", "lastFailure": Record{"stage": "資料の読取", "cause": "原因"}}
	view := s.DraftView(d, auth.Principal{Kind: auth.Screen}, "http://desk.test")
	if record(view["lastFailure"])["cause"] != "原因" {
		t.Fatalf("読取に原因がありません: %+v", view)
	}
}

func TestPublicMessagesAndProblemsStayReadable(t *testing.T) {
	d, _ := failedDraft(t, AtStage("受付AIの呼び出し", &ai.Failure{Kind: "authentication", Message: "AIの契約アカウントの認証を確認してください。"}), true)
	failure := record(d["lastFailure"])
	if d["error"] != "AIの契約アカウントの認証を確認してください。" || failure["kind"] != "authentication" || failure["stage"] != "受付AIの呼び出し" {
		t.Fatalf("公開文か種類が違います: %+v", d)
	}
	d, _ = failedDraft(t, contract.NewProblem(503, "Codex・Claude がともに無効のため、AIを実行しません。", ""), true)
	if d["error"] != "Codex・Claude がともに無効のため、AIを実行しません。" {
		t.Fatalf("受付の断りが固定文に隠れています: %q", d["error"])
	}
}

type temporaryFailure struct{}

func (temporaryFailure) Error() string   { return "temporary fixture" }
func (temporaryFailure) Transient() bool { return true }
func (temporaryFailure) Timeout() bool   { return false }

func TestStageKeepsErrorClassification(t *testing.T) {
	wrapped := AtStage("外側", AtStage("内側", temporaryFailure{}))
	var transient interface{ Transient() bool }
	if !errors.As(wrapped, &transient) || FailureStage(wrapped) != "内側" || wrapped.Error() != "temporary fixture" {
		t.Fatalf("段階の付加が元の失敗を変えています: %v", wrapped)
	}
	if AtStage("段階", nil) != nil || FailureStage(errors.New("x")) != unknownFailureStage {
		t.Fatal("nil と段階なしの扱いが違います")
	}
}

func TestStopBeforeTurnKeepsCauseOnThinkingDraft(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	lines := make(chan string, 8)
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Report: func(err error) { lines <- err.Error() }, Turn: func(context.Context, Record) (Record, error) { return Record{}, nil }, Principal: func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{}, errors.New("送り手の読取に失敗 password=fixture-secret-value")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	id := todoID
	seed := Record{"id": id, "status": "thinking", "owner": "pc-1", "project": "owner/app", "updatedAt": timestamp(s.options.Now())}
	if _, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, _ Record) (Record, error) { return seed, nil }); err != nil {
		t.Fatal(err)
	}
	engine.attempt(ctx, id)
	d, _, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil {
		t.Fatal(err)
	}
	failure := record(d["lastFailure"])
	if d["status"] != "thinking" || failure["stage"] != "送り手の確認" || !strings.Contains(text(failure["cause"]), "送り手の読取に失敗") || strings.Contains(text(failure["cause"]), "fixture-secret-value") {
		t.Fatalf("打ち切りの原因がありません: %+v", d)
	}
	select {
	case line := <-lines:
		if !strings.Contains(line, "送り手の読取に失敗") || strings.Contains(line, "fixture-secret-value") {
			t.Fatalf("ログの内容が違います: %q", line)
		}
	default:
		t.Fatal("ログがありません")
	}
}
