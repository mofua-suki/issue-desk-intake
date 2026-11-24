package startup

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"time"
)

func (a *App) liveAIAlerts(ctx context.Context) ([]any, error) {
	calls, err := a.Runner.Calls(ctx)
	if err != nil {
		return nil, err
	}
	out := []any{}
	now := time.Now()
	for _, call := range calls {
		elapsed := now.Sub(call.StartedAt)
		if elapsed <= 30*time.Minute {
			continue
		}
		out = append(out, map[string]any{"id": "intake-ai-long-running-" + call.ID, "kind": "intake_ai_long_running", "detectedAt": contract.ISO(call.StartedAt.Add(30 * time.Minute)), "title": "受付AIの実行が30分を超えています", "detail": fmt.Sprintf("相談ID: %s／工程: %s／実行先: %s／モデル: %s／開始時刻: %s／経過時間: %dms", call.Draft, call.Step, call.Provider, call.Model, contract.ISO(call.StartedAt), elapsed.Milliseconds())})
	}
	return out, nil
}
