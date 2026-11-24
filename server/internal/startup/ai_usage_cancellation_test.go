package startup

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
	"testing"
	"time"
)

func TestCanceledCallStillPersistsCapturedUsagePostgres(t *testing.T) {
	db := appPostgres(t)
	app := &App{DB: db}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	input, output, read := int64(10), int64(3), int64(2)
	partial := true
	measured := ai.Usage{InputTokens: &input, OutputTokens: &output, CacheReadTokens: &read, Partial: &partial}
	request := ai.Request{Draft: "cancelled-fixture", Project: "owner/repo", Provider: "codex", Model: "gpt-6.1-terra", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1}
	if err := app.recordUsage(ctx, request, measured, time.Second, false); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT json FROM usage_rows WHERE json::jsonb->>'draft'='cancelled-fixture'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	if row["inputTokens"] != float64(10) || row["outputTokens"] != float64(3) || row["cacheReadTokens"] != float64(2) || row["cacheWriteTokens"] != nil || row["usagePartial"] != true || row["outcome"] != "failed" {
		t.Fatal("取消後の実測値か欠測が変わりました", row)
	}
}
