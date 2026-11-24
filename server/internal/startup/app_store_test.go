package startup

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func loadStoreConfig(t *testing.T) (*Config, error) {
	t.Helper()
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	return Load(file, dir, "")
}

func TestAppStoreEnvironmentAcceptsPostgresAndRejectsSQLite(t *testing.T) {
	t.Setenv("ISSUE_DESK_DATABASE_URL", "postgres://u@h/issue_desk_test")
	for _, accepted := range []string{"", "postgres", "Postgres"} {
		t.Setenv("ISSUE_DESK_APP_STORE", accepted)
		config, err := loadStoreConfig(t)
		if err != nil || config.AppPostgresDSN == "" {
			t.Fatalf("%q: config=%+v err=%v", accepted, config, err)
		}
	}
	for _, rejected := range []string{"sqlite", "mysql"} {
		t.Setenv("ISSUE_DESK_APP_STORE", rejected)
		if _, err := loadStoreConfig(t); err == nil {
			t.Fatalf("%q must be rejected", rejected)
		}
	}
}

func TestBusinessAppDoesNotStartExclusivePostgres(t *testing.T) {
	active := openLifecycleApp(t, lifecycleGitea, nil)
	active.StopBackground()
	marker := filepath.Join(active.Config.RuntimeDir, "state", "intake-drain.request")
	if err := os.WriteFile(marker, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	standby, err := OpenBusinessApp(context.Background(), active.Config, active.DB, active.Revision, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer standby.Close()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("非担当が交代状態を削除", err)
	}
	if standby.Engine == nil || standby.currentGate() != nil || standby.backgroundCtx != nil {
		t.Fatal("業務初期化で単独実行が開始された")
	}
	for _, kind := range []string{"ai", "filing", "project"} {
		if _, _, err := standby.acquireGate(context.Background(), kind); err == nil {
			t.Fatalf("非担当の%s枠を取得", kind)
		}
	}
	p := auth.Principal{Kind: auth.Screen}
	_, err = standby.Consultations.Chat(context.Background(), "44444444-4444-4444-8444-444444444444", consultations.Record{"text": "相談", "messageId": "44444444-4444-4444-8444-444444444444"}, p)
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != 503 {
		t.Fatalf("未結合のEngine: %v", err)
	}
}

func TestStopExclusiveKeepsAcceptedTurnPostgres(t *testing.T) {
	existing := openLifecycleApp(t, lifecycleGitea, nil)
	existing.StopBackground()
	app, err := OpenBusinessApp(context.Background(), existing.Config, existing.DB, existing.Revision, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.Engine.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	engine, err := consultations.NewEngine(app.exclusiveCtx, consultations.EngineOptions{Service: app.Consultations, Turn: func(ctx context.Context, _ consultations.Record) (consultations.Record, error) {
		close(started)
		select {
		case <-release:
			return consultations.Record{"reply": "完了"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	app.Engine = engine
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	if err = app.StartExclusive(ownerCtx); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := "33333333-3333-4333-8333-333333333333"
	principal := auth.Principal{Kind: auth.Screen}
	if _, err = app.Consultations.CreateDraft(ctx, consultations.Record{"tier": "light", "id": id, "project": "owner/repo", "hold": true}, principal, false); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Consultations.Chat(ctx, id, consultations.Record{"text": "相談", "messageId": id}, principal); err != nil {
		t.Fatal(err)
	}
	<-started
	done := make(chan error, 1)
	cancelOwner()
	go func() { done <- app.StopExclusive(ctx) }()
	<-app.backgroundCtx.Done()
	if app.ctx.Err() != nil {
		t.Fatal("業務寿命が停止")
	}
	select {
	case err := <-done:
		t.Fatalf("受理済みの処理を待たずに停止: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	d, found, err := app.Records.Get(ctx, "drafts", id)
	if err != nil || !found || !strings.Contains(fmt.Sprint(d["messages"]), "完了") {
		t.Fatalf("応答未保存: %v %v %v", d, found, err)
	}
	if app.ctx.Err() != nil {
		t.Fatal("業務寿命が停止")
	}
}
