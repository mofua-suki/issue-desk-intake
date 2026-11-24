package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/usage"
)

func TestUsageCallsFromProviderEvents(t *testing.T) {
	for _, tc := range []struct {
		name, provider, data string
		want                 int64
	}{
		{"claude", "claude", `{"type":"result","num_turns":3}`, 3},
		{"claude zero", "claude", `{"type":"result","num_turns":0}`, 0},
		{"codex", "codex", "{\"type\":\"turn.completed\"}\n{\"type\":\"turn.completed\"}", 2},
		{"codex failed before completion", "codex", `{"type":"turn.failed"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := Measure(tc.provider, []byte(tc.data))
			if u.Calls == nil || *u.Calls != tc.want {
				t.Fatalf("calls = %v, want %d", u.Calls, tc.want)
			}
			row, err := json.Marshal(u)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			if err := json.Unmarshal(row, &saved); err != nil {
				t.Fatal(err)
			}
			if saved["calls"] != float64(tc.want) {
				t.Fatalf("calls in row = %v", saved["calls"])
			}
		})
	}
	if u := Measure("claude", []byte(`{"type":"result"}`)); u.Calls != nil {
		t.Fatalf("missing calls = %v, want nil", *u.Calls)
	}
}

func TestUsageContextFromCompletedCodexTurns(t *testing.T) {
	data := []byte("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":7}}")
	u := Measure("codex", data)
	if u.ContextMaxTokens == nil || *u.ContextMaxTokens != 7 || u.ContextTotalTokens == nil || *u.ContextTotalTokens != 10 {
		t.Fatalf("context totals = %+v", u)
	}
	u = Measure("codex", append(data, []byte("\n{\"type\":\"turn.started\"}")...))
	if u.ContextMaxTokens != nil || u.ContextTotalTokens != nil {
		t.Fatalf("incomplete context totals = %+v", u)
	}
}

func TestMeasuredCallsPassUsageValidation(t *testing.T) {
	u := Measure("codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":0}}`))
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{}
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	row["at"], row["host"], row["source"], row["kind"] = "2026-11-03T00:00:00.000Z", "nas", "intake", "intake"
	if !usage.ValidUsage(row) || row["calls"] != float64(1) || row["contextMaxTokens"] != float64(0) || row["contextTotalTokens"] != float64(0) {
		t.Fatalf("invalid usage row: %v", row)
	}
}

const resumeSession = "12345678-1234-1234-1234-123456789abc"

// These are synthetic rollout records, including the requester's transcribed
// Worker counters. They are not recordings captured from the intake environment.
func resumeToken(at time.Time, total [3]int64, last *[3]int64) string {
	lastUsage := any(nil)
	if last != nil {
		lastUsage = map[string]any{"input_tokens": last[0], "cached_input_tokens": last[1], "output_tokens": last[2], "cache_write_input_tokens": 0, "reasoning_output_tokens": 0, "total_tokens": last[0] + last[2]}
	}
	row := map[string]any{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{
		"type": "token_count", "info": map[string]any{
			"total_token_usage": map[string]any{"input_tokens": total[0], "cached_input_tokens": total[1], "output_tokens": total[2], "cache_write_input_tokens": 0, "reasoning_output_tokens": 0, "total_tokens": total[0] + total[2]},
			"last_token_usage":  lastUsage, "model_context_window": 475000,
		},
	}}
	b, _ := json.Marshal(row)
	return string(b) + "\n"
}

func resumeLog(t *testing.T, baseline [3]int64) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "11", "04")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-04T00-00-00-"+resumeSession+".jsonl")
	data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", resumeSession)
	data += resumeToken(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), baseline, nil)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendResumeLog(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(data)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("append: %v %v", err, closeErr)
	}
}

// Runs the real Runner.Call accounting path, replacing only CLI execution.
func resumeCall(t *testing.T, values [3]int64, mutate func(), stdoutExtra string, observe func(Usage)) Answer {
	t.Helper()
	request := testRequest(t, "codex")
	request.Resume, request.PersistSession, request.Session = true, true, resumeSession
	runner := Runner{Execute: func(_ context.Context, p ProcessRequest) (ProcessResult, error) {
		if p.Arguments[0] == "login" {
			return ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		if !strings.Contains(strings.Join(p.Arguments, " "), "resume "+resumeSession) {
			t.Fatal("resume argument missing")
		}
		mutate()
		for i, arg := range p.Arguments {
			if arg == "-o" {
				if err := os.WriteFile(p.Arguments[i+1], []byte(`{"ok":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		data := fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":%q}\n{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":%d,\"cached_input_tokens\":%d,\"output_tokens\":%d}}\n", resumeSession, values[0], values[1], values[2])
		return ProcessResult{Stdout: []byte(data + stdoutExtra)}, nil
	}}
	if observe != nil {
		runner.Observe = func(_ Request, u Usage, _ time.Duration, ok bool) {
			if !ok {
				t.Fatal("normal answer observation failed")
			}
			observe(u)
		}
	}
	answer, err := runner.Call(context.Background(), request)
	if err != nil || answer.Value["ok"] != true {
		t.Fatalf("normal answer lost: %+v %v", answer, err)
	}
	return answer
}

func assertResumeTokens(t *testing.T, u Usage, want [3]int64) {
	t.Helper()
	for i, n := range []*int64{u.InputTokens, u.CacheReadTokens, u.OutputTokens} {
		if n == nil || *n != want[i] {
			t.Fatalf("tokens[%d] = %v, want %d; %+v", i, n, want[i], u)
		}
	}
	if u.Calls == nil || *u.Calls != 1 || u.CacheWriteTokens != nil {
		t.Fatalf("calls/cache write changed: %+v", u)
	}
}

func TestCodexResumeUsageDelta(t *testing.T) {
	path := resumeLog(t, [3]int64{900, 700, 50})
	end := [3]int64{1500, 1200, 80}
	answer := resumeCall(t, end, func() { appendResumeLog(t, path, resumeToken(time.Now(), end, nil)) }, "", nil)
	assertResumeTokens(t, answer.Usage, [3]int64{600, 500, 30})
	if answer.Usage.ContextMaxTokens == nil || *answer.Usage.ContextMaxTokens != 600 || answer.Usage.ContextTotalTokens == nil || *answer.Usage.ContextTotalTokens != 600 {
		t.Fatalf("context reused cumulative input: %+v", answer.Usage)
	}
}

func TestCodexResumeUsageWithoutFinalNewline(t *testing.T) {
	for _, baselineUnterminated := range []bool{false, true} {
		t.Run(fmt.Sprint(baselineUnterminated), func(t *testing.T) {
			path := resumeLog(t, [3]int64{900, 700, 50})
			if baselineUnterminated {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.TrimSuffix(string(data), "\n")), 0600); err != nil {
					t.Fatal(err)
				}
			}
			end := [3]int64{1500, 1200, 80}
			answer := resumeCall(t, end, func() {
				separator := ""
				if baselineUnterminated {
					separator = "\n"
				}
				appendResumeLog(t, path, separator+strings.TrimSuffix(resumeToken(time.Now(), end, nil), "\n"))
			}, "", nil)
			assertResumeTokens(t, answer.Usage, [3]int64{600, 500, 30})
		})
	}
}

func TestCodexResumeUsageLargeRolloutLines(t *testing.T) {
	path := resumeLog(t, [3]int64{900, 700, 50})
	large := strings.Repeat("A", 8<<20)
	imageRow := func() string {
		return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"response_item\",\"payload\":{\"content\":[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,%s\"}]}}\n", time.Now().UTC().Format(time.RFC3339Nano), large)
	}
	appendResumeLog(t, path, imageRow())
	end := [3]int64{1500, 1200, 80}
	answer := resumeCall(t, end, func() {
		appendResumeLog(t, path, imageRow()+resumeToken(time.Now(), end, nil))
	}, "", nil)
	assertResumeTokens(t, answer.Usage, [3]int64{600, 500, 30})
	rate := fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"primary\":{\"used_percent\":13,\"window_minutes\":300,\"resets_at\":1800000000}}}}\n", time.Now().UTC().Format(time.RFC3339Nano))
	appendResumeLog(t, path, rate)
	limits := rolloutLimits(resumeSession, time.Now().Add(-time.Minute))
	if len(limits) != 1 || limits[0]["percent"] != float64(13) {
		t.Fatalf("limits after large image: %v", limits)
	}
}

func TestCodexResumeUsageOversizedRolloutIsUnavailable(t *testing.T) {
	path := resumeLog(t, [3]int64{900, 700, 50})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"response_item","payload":{"image":"`); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("A", 1<<20)
	for i := 0; i < 64; i++ {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString("\"}}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	end := [3]int64{1500, 1200, 80}
	answer := resumeCall(t, end, func() { appendResumeLog(t, path, resumeToken(time.Now(), end, nil)) }, "", nil)
	if answer.Usage.InputTokens != nil || answer.Usage.CacheReadTokens != nil || answer.Usage.OutputTokens != nil {
		t.Fatalf("oversized baseline must remain unavailable: %+v", answer.Usage)
	}
}

func TestCodexResumeUsageConsecutiveCalls(t *testing.T) {
	// Requester-provided Worker transcription: codex-cli 0.160.1, 2026-11-04 JST.
	path := resumeLog(t, [3]int64{925480, 889984, 3670})
	firstLast := [3]int64{48421, 47360, 1680}
	appendResumeLog(t, path, resumeToken(time.Date(2026, 11, 4, 0, 1, 0, 0, time.UTC), [3]int64{925480, 889984, 3670}, &firstLast))
	var sum [3]int64
	for i, end := range [][3]int64{{977667, 938240, 5939}, {1038094, 990208, 7535}} {
		last := [][3]int64{{52187, 48256, 2269}, {60427, 51968, 1596}}[i]
		answer := resumeCall(t, end, func() { appendResumeLog(t, path, resumeToken(time.Now(), end, &last)) }, "", nil)
		want := [][3]int64{{52187, 48256, 2269}, {60427, 51968, 1596}}[i]
		assertResumeTokens(t, answer.Usage, want)
		for j, n := range []*int64{answer.Usage.InputTokens, answer.Usage.CacheReadTokens, answer.Usage.OutputTokens} {
			sum[j] += *n
		}
	}
	if sum != [3]int64{112614, 100224, 3865} {
		t.Fatalf("consecutive totals = %v", sum)
	}
}

func TestCodexResumeUsageNonCumulative(t *testing.T) {
	path := resumeLog(t, [3]int64{900, 700, 50})
	answer := resumeCall(t, [3]int64{600, 500, 30}, func() {
		last := [3]int64{40, 30, 10}
		appendResumeLog(t, path, resumeToken(time.Now(), [3]int64{1500, 1200, 80}, &last))
	}, "", nil)
	assertResumeTokens(t, answer.Usage, [3]int64{600, 500, 30})
	if answer.Usage.ContextMaxTokens == nil || *answer.Usage.ContextMaxTokens != 600 || answer.Usage.ContextTotalTokens == nil || *answer.Usage.ContextTotalTokens != 600 {
		t.Fatalf("compatibility context changed: %+v", answer.Usage)
	}
}

func TestCodexResumeUsageContext(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			path := resumeLog(t, [3]int64{900, 700, 50})
			end := [3]int64{1500, 1200, 80}
			answer := resumeCall(t, end, func() {
				first, second := [3]int64{20, 10, 1}, [3]int64{40, 30, 10}
				appendResumeLog(t, path, resumeToken(time.Now(), [3]int64{900, 700, 50}, &second)) // baseline echo is outside context accounting
				appendResumeLog(t, path, resumeToken(time.Now(), [3]int64{1100, 900, 60}, &first))
				lastUsage := &second
				if !available {
					lastUsage = nil
				}
				last := resumeToken(time.Now(), end, lastUsage)
				appendResumeLog(t, path, last+last) // rate-limit update repeats the same info
			}, "", nil)
			max, total := int64(40), int64(60)
			if !available {
				max, total = 600, 600
			}
			if answer.Usage.ContextMaxTokens == nil || *answer.Usage.ContextMaxTokens != max || answer.Usage.ContextTotalTokens == nil || *answer.Usage.ContextTotalTokens != total {
				t.Fatalf("boundary context = %+v, want %d/%d", answer.Usage, max, total)
			}
		})
	}
}

func TestCodexResumeUsageUnavailableBaseline(t *testing.T) {
	for _, scenario := range []string{"missing baseline", "invalid baseline", "baseline field absent", "replaced", "rewritten prefix", "truncated end", "missing end", "invalid end", "end field absent", "wrong metadata", "wrong stdout session", "old timestamp", "future timestamp", "ambiguous rollout", "negative input", "negative cache", "negative output", "multiple completed turns", "unfinished turn", "multiple rollout turns", "malformed end", "symlink replacement"} {
		t.Run(scenario, func(t *testing.T) {
			path := resumeLog(t, [3]int64{900, 700, 50})
			write := func(path, data string) {
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing baseline" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "invalid baseline" || scenario == "baseline field absent" {
				invalid := `{"input_tokens":-1,"cached_input_tokens":700,"output_tokens":50}`
				if scenario == "baseline field absent" {
					invalid = `{"input_tokens":900,"output_tokens":50}`
				}
				appendResumeLog(t, path, fmt.Sprintf("{\"timestamp\":\"2026-11-04T00:01:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":%s}}}\n", invalid))
			}
			if scenario == "wrong metadata" {
				write(path, fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"other\"}}\n%s", resumeToken(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), [3]int64{900, 700, 50}, nil)))
			}
			end := [3]int64{1500, 1200, 80}
			switch scenario {
			case "negative input":
				end[0] = 800
			case "negative cache":
				end[1] = 600
			case "negative output":
				end[2] = 40
			}
			extra := ""
			switch scenario {
			case "wrong stdout session":
				extra = "{\"type\":\"thread.started\",\"thread_id\":\"87654321-1234-1234-1234-123456789abc\"}\n"
			case "multiple completed turns":
				extra = "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":0,\"cached_input_tokens\":0,\"output_tokens\":0}}\n"
			case "unfinished turn":
				extra = "{\"type\":\"turn.started\"}\n"
			}
			answer := resumeCall(t, end, func() {
				if scenario == "missing baseline" {
					write(path, "")
				}
				if scenario == "symlink replacement" {
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+".old", path); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "replaced" {
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					write(path, "")
				}
				if scenario == "rewritten prefix" {
					write(path, "{}\n")
				}
				if scenario == "ambiguous rollout" {
					write(filepath.Join(filepath.Dir(path), "duplicate-"+resumeSession+".jsonl"), "{}\n")
				}
				if scenario == "missing end" {
					return
				}
				at := time.Now()
				if scenario == "old timestamp" {
					at = at.Add(-time.Hour)
				}
				if scenario == "future timestamp" {
					at = at.Add(time.Hour)
				}
				data := resumeToken(at, end, nil)
				if scenario == "multiple rollout turns" {
					context := func(id string) string {
						return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"turn_context\",\"payload\":{\"turn_id\":%q}}\n", at.UTC().Format(time.RFC3339Nano), id)
					}
					data = context("first") + data + context("second")
				}
				if scenario == "malformed end" {
					data += "{bad}\n"
				}
				if scenario == "invalid end" {
					data = strings.Replace(data, `"input_tokens":1500`, `"input_tokens":1.5`, 1)
				}
				if scenario == "end field absent" {
					data = strings.Replace(data, `"cached_input_tokens":1200,`, "", 1)
				}
				if scenario == "truncated end" {
					data = strings.TrimSuffix(data, "}\n")
				}
				appendResumeLog(t, path, data)
			}, extra, nil)
			for key, v := range map[string]*int64{"inputTokens": answer.Usage.InputTokens, "cacheReadTokens": answer.Usage.CacheReadTokens, "outputTokens": answer.Usage.OutputTokens, "contextMaxTokens": answer.Usage.ContextMaxTokens, "contextTotalTokens": answer.Usage.ContextTotalTokens} {
				if v != nil {
					t.Fatalf("%s = %v, want unavailable", key, *v)
				}
			}
			if answer.Usage.Calls == nil {
				t.Fatal("calls discarded")
			}
		})
	}
}

func TestCodexResumeUsageRunnerObservation(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			path := resumeLog(t, [3]int64{900, 700, 50})
			if !known {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			var observed []Usage
			end := [3]int64{1500, 1200, 80}
			answer := resumeCall(t, end, func() {
				if known {
					appendResumeLog(t, path, resumeToken(time.Now(), end, nil))
				}
			}, "", func(u Usage) { observed = append(observed, u) })
			if len(observed) != 1 || !reflect.DeepEqual(observed[0], answer.Usage) {
				t.Fatalf("observe mismatch: %+v %+v", observed, answer.Usage)
			}
			if known {
				assertResumeTokens(t, observed[0], [3]int64{600, 500, 30})
			} else if answer.Usage.InputTokens != nil || answer.Usage.CacheReadTokens != nil || answer.Usage.OutputTokens != nil {
				t.Fatalf("unavailable observation not null: %+v", answer.Usage)
			}
		})
	}
}
