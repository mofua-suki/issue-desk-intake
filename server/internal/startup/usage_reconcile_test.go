package startup

import (
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
	"testing"
	"time"
)

func TestReadDurableIntakeUsagePreservesPartialAndSender(t *testing.T) {
	db := appPostgres(t)
	app := &App{DB: db}
	input, output, cache := int64(10), int64(3), int64(2)
	partial := true
	req := ai.Request{CallID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Draft: "draft", Project: "owner/repo", Provider: "codex", Model: "gpt-6.1-terra", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: 1}
	if err := app.recordUsage(t.Context(), req, ai.Usage{InputTokens: &input, OutputTokens: &output, CacheReadTokens: &cache, Partial: &partial}, time.Second, false); err != nil {
		t.Fatal(err)
	}
	row, err := app.readIntakeCallUsage(t.Context(), req.CallID)
	if err != nil || row["usagePartial"] != true || row["callId"] != req.CallID || row["draft"] != "draft" {
		t.Fatal(row, err)
	}
	if _, err := db.Exec(`UPDATE usage_rows SET sender='other'`); err != nil {
		t.Fatal(err)
	}
	row, err = app.readIntakeCallUsage(t.Context(), req.CallID)
	if err != nil || row != nil {
		t.Fatalf("他の送信元を採用: %+v %v", row, err)
	}
}
