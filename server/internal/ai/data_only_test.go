package ai

import (
	"context"
	"testing"
)

func TestDataOnlyRejectsToolsBeforeLaunching(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		request := testRequest(t, provider)
		request.DataOnly, request.Tools = true, "Read"
		called := false
		runner := Runner{Execute: func(context.Context, ProcessRequest) (ProcessResult, error) {
			called = true
			return ProcessResult{}, nil
		}}
		if _, err := runner.Call(t.Context(), request); err == nil || called {
			t.Fatal("データ分類の道具を起動前に拒否しなかった", provider, err, called)
		}
	}
}
