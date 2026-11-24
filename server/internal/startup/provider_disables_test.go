package startup

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
)

// Exercise the real startup provider callback, rather than an id-shaped HTTP fixture.
func TestProviderReadinessAllowsDisablePostgres(t *testing.T) {
	ctx := context.Background()
	db := appPostgres(t)
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated", true: "unauthenticated"}[unavailable], func(t *testing.T) {
			app := &App{Config: &Config{RuntimeDir: t.TempDir()}, Runner: &ai.Runner{
				Execute: func(_ context.Context, request ai.ProcessRequest) (ai.ProcessResult, error) {
					if unavailable {
						return ai.ProcessResult{Code: 1}, nil
					}
					if request.Arguments[0] == "login" {
						return ai.ProcessResult{Stderr: []byte("Logged in using ChatGPT")}, nil
					}
					return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
				},
			}}
			ready, err := app.modelProviders(ctx)
			if err != nil {
				t.Fatal(err)
			}
			service := &models.Service{DB: db}
			view, err := service.Disables(ctx, ready)
			if err != nil {
				t.Fatal(err)
			}
			for i, raw := range view["providers"].([]any) {
				row := raw.(map[string]any)
				provider := []string{"codex", "claude"}[i]
				if row["id"] != provider || row["name"] == "" || row["provider"] != provider || row["ready"] != !unavailable {
					t.Fatalf("実際の準備状態が表示に届いていません: %v", row)
				}
				reason := "ready"
				if unavailable {
					reason = "authentication"
				}
				if row["reason"] != reason {
					t.Fatalf("認証状態の理由が失われています: %v", row)
				}
			}
			for _, provider := range []string{"codex", "claude"} {
				_, err := service.SetDisable(ctx, provider, map[string]any{}, "test", ready)
				if unavailable {
					if err == nil {
						t.Fatal("未認証の代替先への切り替えが通りました")
					}
				} else {
					if err != nil {
						t.Fatalf("認証済みの代替先なのに無効化が失敗: %v", err)
					}
					if _, err = service.CancelDisable(ctx, provider); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
