package startup

import (
	"context"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"testing"
	"time"
)

func TestAppShutdownWaitsForAcceptedAIAndRecoversDeadlineWork(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "期限で中断"
		if complete {
			name = "受理済みを完了"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			env := pgtest.NewIsolated(t)
			local := openAppStore(t, env)
			records, e := consultations.OpenRecords(ctx, local.DB())
			if e != nil {
				t.Fatal(e)
			}
			service, e := consultations.New(consultations.Options{Records: records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }})
			if e != nil {
				t.Fatal(e)
			}
			started, finish, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			engine, e := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: func(run context.Context, d consultations.Record) (consultations.Record, error) {
				close(started)
				select {
				case <-finish:
					return consultations.Record{"reply": "受理済みの応答が完了"}, nil
				case <-run.Done():
					close(canceled)
					return nil, run.Err()
				}
			}})
			if e != nil {
				t.Fatal(e)
			}
			service.BindEngine(engine)
			app := App{ctx: ctx, cancel: cancel, Engine: engine, SQLite: local}
			defer app.Close()
			id := "44444444-4444-4444-4444-444444444444"
			p := auth.Principal{Kind: auth.Screen}
			if _, e = service.CreateDraft(ctx, consultations.Record{"tier": "light", "id": id, "project": "owner/repo", "hold": true}, p, false); e != nil {
				t.Fatal(e)
			}
			if _, e = service.Chat(ctx, id, consultations.Record{"text": "受理した処理", "messageId": id}, p); e != nil {
				t.Fatal(e)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("AI起動なし")
			}
			d, _, e := records.Get(ctx, "drafts", id)
			if e != nil {
				t.Fatal(e)
			}
			grace, stop := context.WithCancel(context.Background())
			defer stop()
			done := make(chan struct{})
			go func() { app.Shutdown(grace); close(done) }()
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			for {
				if _, e = engine.Start(ctx, id, d, p); e != nil {
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-deadline.C:
					t.Fatal("shutdown中も新しいAIを受理")
				}
			}
			select {
			case <-canceled:
				t.Fatal("猶予中の受理済みAIを即取消")
			default:
			}
			if complete {
				close(finish)
			} else {
				stop()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("終了が完了しない")
			}
			saved := openAppStore(t, env)
			reopened, e := consultations.OpenRecords(context.Background(), saved.DB())
			if e != nil {
				t.Fatal(e)
			}
			result, _, e := reopened.Get(context.Background(), "drafts", id)
			if e != nil {
				t.Fatal(e)
			}
			if complete {
				if result["status"] != "draft" || len(fleet.List(result["messages"])) != 2 {
					t.Fatal("完了した応答を失った", result)
				}
			} else if result["status"] != "thinking" {
				t.Fatal("期限で止めた相談を再開できない", result)
			}
		})
	}
}
