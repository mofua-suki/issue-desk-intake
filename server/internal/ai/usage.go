package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

type Usage struct {
	ServiceTier               string            `json:"serviceTier,omitempty"`
	DiagnosticKind            string            `json:"diagnosticKind,omitempty"`
	FirstOutputMs             *int64            `json:"firstOutputMs,omitempty"`
	Calls                     *int64            `json:"calls"`
	ContextMaxTokens          *int64            `json:"contextMaxTokens"`
	ContextTotalTokens        *int64            `json:"contextTotalTokens"`
	PromptBytes               map[string]*int64 `json:"promptBytes,omitempty"`
	PromptTotalBytes          *int64            `json:"promptTotalBytes"`
	ContextReads              map[string]any    `json:"contextReads"`
	MeasurementMissingReasons map[string]string `json:"measurementMissingReasons,omitempty"`
	SessionCostUSD            *float64          `json:"sessionCostUsd"`
	CostUnknownReason         *string           `json:"costUnknownReason"`
	InputTokens               *int64            `json:"inputTokens"`
	OutputTokens              *int64            `json:"outputTokens"`
	CacheReadTokens           *int64            `json:"cacheReadTokens"`
	CacheWriteTokens          *int64            `json:"cacheWriteTokens"`
	CostUSD                   *float64          `json:"costUsd"`
	Partial                   *bool             `json:"usagePartial"`
}

// fleet.Object を使わない: reviewnotes のテストが ai を import し、fleet が reviewnotes を import するため循環する。
func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func events(data []byte) []map[string]any {
	decode := func(input []byte) any {
		var value any
		d := json.NewDecoder(bytes.NewReader(input))
		d.UseNumber()
		if d.Decode(&value) != nil {
			return nil
		}
		return value
	}
	whole := decode(data)
	if list, ok := whole.([]any); ok {
		result := []map[string]any{}
		for _, value := range list {
			if row := object(value); row != nil {
				result = append(result, row)
			}
		}
		return result
	}
	if row := object(whole); row != nil && !bytes.Contains(bytes.TrimSpace(data), []byte{'\n'}) {
		return []map[string]any{row}
	}
	result := []map[string]any{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if row := object(decode(line)); row != nil {
			result = append(result, row)
		}
	}
	if len(result) == 0 && object(whole) != nil {
		return []map[string]any{object(whole)}
	}
	return result
}
func count(value any) *int64 {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	n, err := number.Int64()
	if err != nil || n < 0 || n > 9007199254740991 {
		return nil
	}
	return &n
}
func amount(value any) *float64 {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	n, err := number.Float64()
	if err != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return nil
	}
	return &n
}
func sum(rows []map[string]any, key string) *int64 {
	if len(rows) == 0 {
		return nil
	}
	var total int64
	for _, row := range rows {
		n := count(row[key])
		if n == nil || total > 9007199254740991-*n {
			return nil
		}
		total += *n
	}
	return &total
}
func Measure(provider string, data []byte) Usage {
	rows := events(data)
	if provider == "codex" {
		return measureCodex(rows)
	}
	if provider != "claude" {
		return Usage{}
	}
	return measureClaude(rows)
}

// measureCodex totals the completed turns; a failed or unfinished turn makes the measurement partial.
func measureCodex(rows []map[string]any) Usage {
	usage := Usage{}
	turns := []map[string]any{}
	open, incomplete, observed := 0, false, false
	for _, row := range rows {
		switch row["type"] {
		case "turn.started":
			observed = true
			open++
		case "turn.failed":
			observed = true
			incomplete = true
			if open > 0 {
				open--
			}
		case "turn.completed":
			observed = true
			if open > 0 {
				open--
			}
			turns = append(turns, object(row["usage"]))
		}
	}
	if len(turns) == 0 {
		if observed {
			zero := int64(0)
			return Usage{Calls: &zero}
		}
		return usage
	}
	partial := incomplete || open > 0
	calls := int64(len(turns))
	var maximum *int64
	total := sum(turns, "input_tokens")
	if total != nil && !partial {
		var max int64
		for _, turn := range turns {
			if n := count(turn["input_tokens"]); *n > max {
				max = *n
			}
		}
		maximum = &max
	} else {
		total = nil
	}
	return Usage{Calls: &calls, ContextMaxTokens: maximum, ContextTotalTokens: total, InputTokens: sum(turns, "input_tokens"), OutputTokens: sum(turns, "output_tokens"), CacheReadTokens: sum(turns, "cached_input_tokens"), Partial: &partial}
}

// measureClaude reads the final result, or else sums the assistant messages as a partial measurement.
func measureClaude(rows []map[string]any) Usage {
	usage := Usage{}
	var result map[string]any
	for _, row := range rows {
		if row["type"] == "result" {
			result = row
		}
	}
	if result != nil {
		u := object(result["usage"])
		partial := false
		return Usage{Calls: count(result["num_turns"]), InputTokens: count(u["input_tokens"]), OutputTokens: count(u["output_tokens"]), CacheReadTokens: count(u["cache_read_input_tokens"]), CacheWriteTokens: count(u["cache_creation_input_tokens"]), CostUSD: amount(result["total_cost_usd"]), Partial: &partial}
	}
	messages := map[string]map[string]any{}
	for index, row := range rows {
		if row["type"] != "assistant" {
			continue
		}
		message := object(row["message"])
		u := object(message["usage"])
		if u == nil {
			continue
		}
		id, _ := message["id"].(string)
		if id == "" {
			id = "anonymous:" + strconv.Itoa(index)
		}
		messages[id] = u
	}
	partials := []map[string]any{}
	for _, row := range messages {
		partials = append(partials, row)
	}
	if len(partials) == 0 {
		return usage
	}
	partial := true
	return Usage{InputTokens: sum(partials, "input_tokens"), OutputTokens: sum(partials, "output_tokens"), CacheReadTokens: sum(partials, "cache_read_input_tokens"), CacheWriteTokens: sum(partials, "cache_creation_input_tokens"), Partial: &partial}
}
func measureCodexResume(u Usage, data []byte, id string, base *rolloutSnapshot, started, ended time.Time) Usage {
	missing := func() Usage {
		u.InputTokens, u.CacheReadTokens, u.OutputTokens = nil, nil, nil
		u.ContextMaxTokens, u.ContextTotalTokens = nil, nil
		u.MeasurementMissingReasons = map[string]string{"contextMaxTokens": "provider_unavailable", "contextTotalTokens": "provider_unavailable"}
		return u
	}
	// Multiple completed turns cannot be matched to one terminal cumulative
	// measurement by summing their usage. Preserve calls, but do not guess tokens.
	if u.Calls == nil || *u.Calls != 1 || u.Partial == nil || *u.Partial {
		return missing()
	}
	identity := false
	for _, row := range events(data) {
		if row["type"] == "thread.started" {
			if row["thread_id"] != id {
				return missing()
			}
			identity = true
		}
	}
	if !identity || u.InputTokens == nil || u.CacheReadTokens == nil || u.OutputTokens == nil {
		return missing()
	}
	end := base.finish(id, started, ended)
	if end == nil {
		return missing()
	}
	values := [3]int64{*u.InputTokens, *u.CacheReadTokens, *u.OutputTokens}
	if values != end.total {
		return u // Verified log boundary, but this CLI reports per-call usage.
	}
	for i := range values {
		if values[i] < base.total[i] {
			return missing() // Never escape a negative delta through compatibility.
		}
		values[i] -= base.total[i]
	}
	u.InputTokens, u.CacheReadTokens, u.OutputTokens = &values[0], &values[1], &values[2]
	u.ContextMaxTokens, u.ContextTotalTokens = end.contextMax, end.contextTotal
	if u.ContextMaxTokens == nil || u.ContextTotalTokens == nil {
		u.ContextMaxTokens, u.ContextTotalTokens = &values[0], &values[0]
	}
	return u
}

func ClaudeAnswer(data []byte) ([]byte, error) {
	var result map[string]any
	for _, row := range events(data) {
		if row["type"] == "result" {
			result = row
		}
	}
	if result == nil || result["is_error"] == true {
		return nil, errors.New("Claudeが正常な構造化回答を返しませんでした")
	}
	if answer := object(result["structured_output"]); answer != nil {
		return json.Marshal(answer)
	}
	if text, ok := result["result"].(string); ok {
		answer := strings.TrimSpace(text)
		if strings.HasPrefix(answer, "```") {
			answer = strings.TrimPrefix(answer, "```json")
			answer = strings.TrimPrefix(answer, "```")
			answer = strings.TrimSuffix(answer, "```")
		}
		return []byte(strings.TrimSpace(answer)), nil
	}
	return nil, errors.New("Claudeの構造化回答がありません")
}
