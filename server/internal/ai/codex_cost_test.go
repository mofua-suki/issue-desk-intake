package ai

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"issue-desk-intake/server/internal/usage"
)

// A Codex run reports tokens only; the stored row must still be priceable.
func TestCodexRunUsageRowIsPricedByStatistics(t *testing.T) {
	out := "{\"type\":\"thread.started\",\"thread_id\":\"019a0000-0000-7000-8000-000000000000\"}\n" +
		"{\"type\":\"turn.started\"}\n" +
		"{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":120000,\"cached_input_tokens\":100000,\"output_tokens\":3000}}\n"
	data, err := json.Marshal(callUsage(Request{Provider: "codex", Model: "gpt-6.2-sol"}, ProcessResult{Stdout: []byte(out)}, nil, "", nil, time.Time{}, time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err = json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	if row["costUsd"] != nil {
		t.Fatalf("Codex reported no cost, recorded %v", row["costUsd"])
	}
	row["provider"], row["model"] = "codex", "gpt-6.2-sol"
	rows, _ := usage.CorrectCosts([]map[string]any{row}, nil)
	// 20K uncached x $2 + 100K cached x $0.10 + 3K output x $10, per 1M tokens.
	if cost, ok := rows[0]["costUsd"].(float64); !ok || math.Abs(cost-(0.04+0.01+0.03)) > 1e-9 {
		t.Fatalf("costUsd = %v", rows[0]["costUsd"])
	}
}
