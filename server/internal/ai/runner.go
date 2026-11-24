package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/intakestep"
)

type Request struct {
	CallID          string
	Session         string
	PersistSession  bool
	Resume          bool
	BeforeCall      func() error       `json:"-"`
	OnSession       func(string) error `json:"-"`
	LastSessionCost *float64
	MaxInputChars   int
	MaxInputBytes   int
	Draft           string
	Project         string
	Step            string
	CallTrigger     string
	RepairKind      string
	RepairField     string
	Attempt         int
	Turn            int
	Tier            string
	Provider        string
	Model           string
	Effort          string
	Input           string
	SchemaFile      string
	Workspace       string
	Tools           string
	DataOnly        bool
	Images          []string
	Timeout         time.Duration
	IdleTimeout     time.Duration
	OutputTimeout   time.Duration
}

type Answer struct {
	Session string
	Value   map[string]any
	Usage   Usage
	Elapsed time.Duration
	// Diagnostic is filled for every call, also when it fails. It holds no prompt, answer or credential.
	Diagnostic Diagnostic
}

type Runner struct {
	Codex    string
	Claude   string
	Security Security
	Execute  Execute
	// Observe receives measurements even when execution or schema validation fails.
	// It receives no prompt, response, stderr, or credential material.
	Observe       func(Request, Usage, time.Duration, bool)
	ObserveResult func(Observation)
	ObserveLimits func([]LimitObservation)
}

func (r *Runner) Call(ctx context.Context, request Request) (answer Answer, err error) {
	startedAt := time.Now()
	defer func() {
		if r.ObserveResult != nil {
			o := observation(request, startedAt, err)
			o.DiagnosticKind, o.DiagnosticID, o.Effort = answer.Diagnostic.Kind, answer.Diagnostic.ID, request.Effort
			r.ObserveResult(o)
		}
	}()
	stage, ran := "preflight", false
	var process ProcessResult
	defer func() {
		answer.Diagnostic = diagnose(stage, process, ran, err, startedAt)
		if ran && process.Cause == "spawn_failed" && runWasNotStarted(request, process, err) {
			answer.Diagnostic.UsageEvidence = "process_not_started"
		} else if ran && err != nil && evidenceStep(request.Step) && measuredZero(request.Provider, answer.Usage) && !process.Truncated {
			answer.Diagnostic.UsageEvidence = "measured_zero"
		}
	}()
	if err = checkRequest(request); err != nil {
		return answer, err
	}
	schemaBytes, err := readSchemaWithinLimit(request)
	if err != nil {
		return answer, err
	}
	schema, err := Compile(request.SchemaFile)
	if err != nil {
		return answer, failed("schema", "AI回答のスキーマが不正です", false)
	}
	run := r.Execute
	if run == nil {
		run = Run
	}
	work, err := os.MkdirTemp(request.Workspace, ".answer-")
	if err != nil {
		return answer, errors.New("AI回答の一時領域を作れません")
	}
	defer os.RemoveAll(work)
	output := filepath.Join(work, "answer.json")
	var exe string
	var args []string
	if request.Provider == "codex" {
		transport := request
		transport.SchemaFile, err = writeCodexSchema(work, schemaBytes)
		if err != nil {
			return answer, err
		}
		exe, args, err = r.codexCommand(ctx, run, transport, output)
	} else {
		exe, args, err = r.claudeCommand(ctx, run, request)
	}
	if err != nil {
		return answer, err
	}
	if request.Session != "" && !sessionID.MatchString(request.Session) {
		return answer, errors.New("AIの会話IDが不正です")
	}
	if request.BeforeCall != nil {
		if err = request.BeforeCall(); err != nil {
			return answer, err
		}
	}
	finishLive := r.live(request)
	defer finishLive()
	var baseline *rolloutSnapshot
	if request.Provider == "codex" && request.Resume && request.Session != "" {
		baseline = snapshotRollout(request.Session)
	}
	stage = "spawn"
	started := time.Now()
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sessionErr error
	line := func(raw string) {
		id := sessionFrom([]byte(raw))
		if id == "" || id == answer.Session {
			return
		}
		answer.Session = id
		if request.OnSession != nil {
			if err := request.OnSession(id); err != nil {
				sessionErr = err
				cancel()
			}
		}
	}
	process, runErr := run(callCtx, ProcessRequest{Executable: exe, Arguments: args, Directory: request.Workspace, Input: request.Input, Timeout: request.Timeout, IdleTimeout: request.IdleTimeout, OutputTimeout: request.OutputTimeout, OnLine: line, ClaudePartial: request.Provider == "claude" && request.OutputTimeout > 0})
	ended := time.Now()
	ran, stage = true, "exit"
	if answer.Session == "" {
		line(string(process.Stdout))
	}
	answer.Elapsed = time.Since(started)
	r.observeLimits(request, answer.Session, process.Stdout, started)
	answer.Usage = callUsage(request, process, runErr, answer.Session, baseline, started, ended)
	defer func() {
		if r.Observe != nil {
			observed := request
			observed.Input = ""
			observed.Images = nil
			observed.BeforeCall, observed.OnSession = nil, nil
			r.Observe(observed, answer.Usage, answer.Elapsed, err == nil)
		}
	}()
	if sessionErr != nil {
		return answer, failed("session", "会話の復旧情報を保存できないため停止しました", false)
	}
	if runErr != nil && (process.Cause != "exit_nonzero" || process.Truncated) {
		return answer, runErr
	}
	if process.Code != 0 || process.Truncated {
		return answer, exitFailure(process)
	}
	stage = "answer"
	data, err := readAnswer(request.Provider, output, process.Stdout)
	if err != nil {
		return answer, err
	}
	answer.Value, err = Validate(schema, data)
	if err != nil {
		failure := failed("format", "AIの回答が指定の形式に一致しません", false)
		failure.Field = validationField(schema, schemaBytes, data)
		err = failure
	}
	if err == nil {
		stage = "done"
	}
	return answer, err
}

// checkRequest rejects an unknown provider, a missing model or workspace, and
// tools other than the read-only ones.
func checkRequest(request Request) error {
	if request.Provider != "codex" && request.Provider != "claude" {
		return errors.New("相談の実行先が不正です")
	}
	if request.Model == "" || request.Workspace == "" {
		return errors.New("相談のモデルと作業領域が必要です")
	}
	if request.DataOnly && request.Tools != "" {
		return errors.New("データ分類には道具を渡せません")
	}
	for _, tool := range strings.Split(request.Tools, ",") {
		if tool != "" && tool != "Read" && tool != "Glob" && tool != "Grep" {
			return errors.New("相談で使う道具は読み取り専用です")
		}
	}
	return nil
}

// readSchemaWithinLimit reads the answer schema and checks that it fits in the
// input limit together with the prompt.
func readSchemaWithinLimit(request Request) ([]byte, error) {
	schemaBytes, readErr := os.ReadFile(request.SchemaFile)
	if readErr != nil {
		return nil, failed("schema", "AI回答のスキーマを読み取れません", false)
	}
	maxBytes, maxChars := request.MaxInputBytes, request.MaxInputChars
	if maxBytes < 1 {
		maxBytes = 1048576
	}
	if maxChars < 1 {
		maxChars = 524288
	}
	if len(request.Input)+len(schemaBytes) > maxBytes || contract.UTF16Length(request.Input)+contract.UTF16Length(string(schemaBytes)) > maxChars {
		return nil, failed("input_size", "AIへ送る内容が上限を超えています。同じ相談で資料と未確認事項を確認してください。", false)
	}
	return schemaBytes, nil
}

// codexCommand checks that Codex is ready and builds its command line.
func (r *Runner) codexCommand(ctx context.Context, run Execute, request Request, output string) (string, []string, error) {
	exe := r.Codex
	var args []string
	if exe == "" {
		exe = "codex"
	}
	if err := r.codexReady(ctx, run, exe, request.Workspace); err != nil {
		return "", nil, err
	}
	args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check"}
	if !request.PersistSession {
		args = append(args, "--ephemeral")
	}
	security := r.Security
	security.ToolExecutables = append(append([]string{}, security.ToolExecutables...), exe)
	args = append(args, policy(security, request.Workspace, request.DataOnly)...)
	args = append(args, "-m", request.Model, "-c", "model_reasoning_effort="+quoted(request.Effort))
	if tier := codexServiceTier(request.Model); tier != "" {
		args = append(args, "-c", "service_tier="+quoted(tier))
	}
	args = append(args, "--json", "--output-schema", request.SchemaFile, "-o", output, "-C", request.Workspace)
	for _, image := range request.Images {
		args = append(args, "--image", image)
	}
	if request.Resume && request.Session != "" {
		args = append(args, "resume", request.Session)
	}
	args = append(args, "-")
	return exe, args, nil
}

// claudeCommand checks that Claude is ready and builds its command line.
func (r *Runner) claudeCommand(ctx context.Context, run Execute, request Request) (string, []string, error) {
	var args []string
	exe := r.Claude
	if exe == "" {
		exe = "claude"
	}
	if err := r.claudeReady(ctx, run, exe, request.Workspace); err != nil {
		return "", nil, err
	}
	document, readErr := os.ReadFile(request.SchemaFile)
	if readErr != nil {
		return "", nil, errors.New("AI回答のスキーマを読み取れません")
	}
	settings, marshalErr := json.Marshal(ClaudeSettings(r.Security, request.Workspace))
	if marshalErr != nil {
		return "", nil, errors.New("相談の読み取り設定を作れません")
	}
	args = []string{"-p", "--model", request.Model, "--effort", request.Effort, "--tools", request.Tools,
		"--restricted", "--permission-mode", "dontAsk", "--setting-sources", "", "--settings", string(settings),
		"--strict-mcp-config", "--disable-slash-commands", "--no-chrome",
		"--output-format", "stream-json", "--verbose", "--json-schema", string(document)}
	if request.OutputTimeout > 0 {
		args = append(args, "--include-partial-messages")
	}
	if !request.PersistSession {
		args = append(args, "--no-session-persistence")
	} else if request.Session != "" {
		flag := "--session-id"
		if request.Resume {
			flag = "--resume"
		}
		args = append(args, flag, request.Session)
	}
	return exe, args, nil
}

// observeLimits reports the rate limits the call revealed.
func (r *Runner) observeLimits(request Request, session string, stdout []byte, started time.Time) {
	if r.ObserveLimits != nil {
		var limits []LimitObservation
		if request.Provider == "claude" {
			limits = claudeLimits(stdout)
		} else if request.PersistSession {
			id := session
			if id == "" && request.Resume {
				id = request.Session
			}
			limits = rolloutLimits(id, started)
		}
		if len(limits) > 0 {
			r.ObserveLimits(limits)
		}
	}
}

func codexServiceTier(model string) string {
	switch model {
	case "gpt-6.1-terra", "gpt-6.2-sol", "gpt-6.2-astra":
		return "default"
	default:
		return ""
	}
}

// callUsage measures the call's usage from its output, correcting it for a
// resumed Codex session, a process that never started, truncated output and
// the Claude session cost.
func callUsage(request Request, process ProcessResult, runErr error, session string, baseline *rolloutSnapshot, started, ended time.Time) Usage {
	usage := Measure(request.Provider, process.Stdout)
	if request.Provider == "codex" && request.Resume && request.Session != "" {
		usage = measureCodexResume(usage, process.Stdout, request.Session, baseline, started, ended)
	}
	if runWasNotStarted(request, process, runErr) {
		zero := int64(0)
		complete := false
		usage = Usage{Calls: &zero, InputTokens: &zero, OutputTokens: &zero, CacheReadTokens: &zero, Partial: &complete}
		if request.Provider == "claude" {
			usage.CacheWriteTokens = &zero
		}
	} else if request.Provider == "codex" {
		usage.ServiceTier = codexServiceTier(request.Model)
	}
	if process.Truncated {
		partial := true
		usage.Partial = &partial
	}
	if request.Provider == "claude" && request.PersistSession {
		usage = SessionCost(usage, request.LastSessionCost, request.Resume, session != "" && request.Session != "" && session != request.Session)
	}
	return usage
}

// exitFailure classifies a failed or truncated process by its output.
func exitFailure(process ProcessResult) error {
	diagnostic := string(process.Stdout) + "\n" + string(process.Stderr)
	if regexp.MustCompile(`(?i)unauthorized|unauthenticated|authentication|oauth|not logged in|invalid.api.key|login`).MatchString(diagnostic) {
		return failed("authentication", "AIの契約アカウントの認証を確認してください。", false)
	}
	if regexp.MustCompile(`(?i)usage.limit|rate.limit|too.many.requests|\b429\b`).MatchString(diagnostic) {
		if regexp.MustCompile(`(?i)rate.limit|too.many.requests|\b429\b`).MatchString(diagnostic) && !regexp.MustCompile(`(?i)usage.limit|hit.your.limit|weekly.limit|daily.limit`).MatchString(diagnostic) {
			return failed("rate_limit", "AIへの通信が一時的に制限されています。待って同じ呼び出しを続けます。", true)
		}
		return failed("usage_limit", "AIの利用上限に達しています。回復後に再試行してください。", false)
	}
	return failed("execution", "AIが正常な回答を返さず終了しました", false)
}

// readAnswer reads the structured answer: Codex writes it to the output file,
// Claude prints it in its stream.
func readAnswer(provider, output string, stdout []byte) ([]byte, error) {
	var data []byte
	var err error
	if provider == "codex" {
		file, openErr := os.Open(output)
		if openErr != nil {
			return nil, errors.New("AIの回答ファイルがありません")
		}
		defer file.Close()
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
			return nil, errors.New("AIの回答ファイルが不正です")
		}
		data, err = os.ReadFile(output)
		if err != nil {
			return nil, errors.New("AIの回答ファイルを読み取れません")
		}
	} else {
		data, err = ClaudeAnswer(stdout)
		if err != nil {
			return nil, failed("format", "AIの構造化回答を確認できません", false)
		}
	}
	return data, nil
}

func runWasNotStarted(request Request, process ProcessResult, err error) bool {
	return err != nil && process.Cause == "spawn_failed" && !process.Truncated &&
		evidenceStep(request.Step)
}

func evidenceStep(step string) bool {
	return step == intakestep.Draft || step == intakestep.FilingCheck
}

func measuredZero(provider string, u Usage) bool {
	zero := func(n *int64) bool { return n != nil && *n == 0 }
	return (provider == "codex" || provider == "claude") && u.Partial != nil && !*u.Partial &&
		zero(u.InputTokens) && zero(u.OutputTokens) && zero(u.CacheReadTokens) &&
		(provider != "claude" || zero(u.CacheWriteTokens))
}

func (r *Runner) claudeReady(ctx context.Context, run Execute, exe, cwd string) error {
	result, err := run(ctx, ProcessRequest{Executable: exe, Arguments: []string{"auth", "status", "--json"}, Directory: cwd, Timeout: 30 * time.Second, IdleTimeout: 30 * time.Second})
	if err != nil || result.Code != 0 || result.Truncated {
		return errors.New("Claudeの契約アカウントの認証を確認できません")
	}
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		APIProvider string `json:"apiProvider"`
		AuthMethod  string `json:"authMethod"`
	}
	if json.Unmarshal(result.Stdout, &status) != nil || !status.LoggedIn || status.APIProvider != "firstParty" || (status.AuthMethod != "oauth_token" && status.AuthMethod != "claude.ai") {
		return errors.New("Claudeの契約アカウントでのログインが必要です")
	}
	return nil
}

func (r *Runner) codexReady(ctx context.Context, run Execute, exe, cwd string) error {
	result, err := run(ctx, ProcessRequest{Executable: exe, Arguments: []string{"login", "status"}, Directory: cwd, Timeout: 20 * time.Second, IdleTimeout: 20 * time.Second})
	if err != nil {
		return err
	}
	if result.Code != 0 || result.Truncated || !strings.Contains(string(result.Stdout)+string(result.Stderr), "ChatGPT") {
		return failed("authentication", "CodexのChatGPTアカウントでのログインが必要です。", false)
	}
	return nil
}
