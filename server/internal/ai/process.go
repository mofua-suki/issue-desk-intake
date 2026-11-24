package ai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProcessRequest struct {
	Executable    string
	Arguments     []string
	Directory     string
	Input         string
	Timeout       time.Duration
	IdleTimeout   time.Duration
	OutputTimeout time.Duration
	OnLine        func(string)
	ClaudePartial bool
}
type ProcessResult struct {
	Stdout    []byte
	Stderr    []byte `json:"-"`
	Code      int
	Truncated bool
	// Cause and the fields below are observations for Diagnostic. Cause is one of timeout_idle, timeout_total,
	// canceled_by_server, spawn_failed, output_overflow, exit_nonzero, or empty when the process ended normally.
	// FirstOutput and LastOutput are offsets from the start; negative when nothing was written.
	Cause       string        `json:"-"`
	Duration    time.Duration `json:"-"`
	FirstOutput time.Duration `json:"-"`
	LastOutput  time.Duration `json:"-"`
	StdoutBytes int           `json:"-"`
	StderrBytes int           `json:"-"`
	// Idle is what the process group was doing when it was killed for silence; nil for any other end.
	Idle          *IdleSnapshot `json:"-"`
	IdleCondition string        `json:"-"`
}
type Execute func(context.Context, ProcessRequest) (ProcessResult, error)

func CleanEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true, "CODEX_HOME": true, "CLAUDE_CONFIG_DIR": true}
	result := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			result = append(result, entry)
		}
	}
	return result
}

type capture struct {
	mu       sync.Mutex
	data     bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
	line     []byte
	onLine   func(string)
	activity chan struct{}
	// total counts every byte written (also past the limit); first and last are the write times.
	total       int
	first, last time.Time
}

func (c *capture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := len(data)
	now := time.Now()
	if c.first.IsZero() {
		c.first = now
	}
	c.last = now
	c.total += size
	select {
	case c.activity <- struct{}{}:
	default:
	}
	if c.overflow {
		return size, nil
	}
	if c.data.Len()+size > c.limit {
		c.overflow = true
		c.cancel()
		return size, nil
	}
	c.data.Write(data)
	if c.onLine != nil {
		c.line = append(c.line, data...)
		for {
			index := bytes.IndexByte(c.line, '\n')
			if index < 0 {
				break
			}
			c.onLine(string(c.line[:index]))
			c.line = c.line[index+1:]
		}
	}
	return size, nil
}

// Run never invokes a shell and never exposes raw provider stderr. A separate
// process group makes timeout/cancellation terminate descendants as well.
func Run(parent context.Context, request ProcessRequest) (ProcessResult, error) {
	return runWithSampler(parent, request, sampleStartedGroup)
}

func runWithSampler(parent context.Context, request ProcessRequest, sampleGroup func(int) groupSample) (ProcessResult, error) {
	if request.Executable == "" || request.Directory == "" {
		return ProcessResult{}, errors.New("AIの実行ファイルと作業領域が必要です")
	}
	if request.Timeout <= 0 {
		request.Timeout = 5 * time.Minute
	}
	if request.IdleTimeout <= 0 {
		request.IdleTimeout = 240 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, request.Timeout)
	defer cancel()
	activity := make(chan struct{}, 1)
	out := &capture{limit: 8 << 20, cancel: cancel, onLine: request.OnLine, activity: activity}
	stderr := &capture{limit: 64 << 10, cancel: cancel, activity: activity}
	var stdout io.Writer = out
	var partial *claudePartialWriter
	if request.ClaudePartial {
		partial = &claudePartialWriter{out: out}
		stdout = partial
	}
	cmd := processCommand(ctx, request, stdout, stderr)
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return ProcessResult{Cause: "spawn_failed", FirstOutput: -1, LastOutput: -1}, errors.New("AIの実行ファイルを起動できません")
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	idle := time.NewTimer(request.IdleTimeout)
	defer idle.Stop()
	var outputDeadline <-chan time.Time
	var outputTimer *time.Timer
	if request.OutputTimeout > 0 {
		outputTimer = time.NewTimer(request.OutputTimeout)
		outputDeadline = outputTimer.C
		defer outputTimer.Stop()
	}
	resetOutput := func() {
		if outputTimer != nil {
			if !outputTimer.Stop() {
				select {
				case <-outputTimer.C:
				default:
				}
			}
			outputTimer.Reset(request.OutputTimeout)
		}
	}
	var err error
	idleCondition := ""
	var idleSnapshot *IdleSnapshot
	// Keep the diagnostic baseline separate from the rolling activity samples.
	sampler := time.NewTicker(idleSampleInterval(request.IdleTimeout))
	defer sampler.Stop()
	var baseline *groupSample
	sampleDue := true
	previous := sampleGroup(cmd.Process.Pid)
	checkActivity := func() groupSample {
		current := sampleGroup(cmd.Process.Pid)
		if groupMadeProgress(previous, current) {
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(request.IdleTimeout)
		}
		previous = current
		return current
	}
loop:
	for {
		select {
		case err = <-done:
			break loop
		case <-activity:
			resetOutput()
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(request.IdleTimeout)
			sampleDue = true
		case <-outputDeadline:
			// Timer and output can become ready together. Use capture timestamps,
			// rather than channel delivery order, to decide whether silence expired.
			last := lastOutputAt(out, stderr, startedAt)
			if remaining := request.OutputTimeout - time.Since(last); remaining > 0 {
				outputTimer.Reset(remaining)
				continue
			}
			if parent.Err() != nil {
				err = <-done
				break loop
			}
			idleCondition = "output_deadline"
			current := sampleGroup(cmd.Process.Pid)
			idleSnapshot = newIdleSnapshot(current, baseline)
			cancel()
			err = <-done
			break loop
		case <-sampler.C:
			sample := checkActivity()
			if sampleDue {
				baseline, sampleDue = &sample, false
			}
		case <-idle.C:
			// A write may have arrived while the timer and activity were both ready.
			select {
			case <-activity:
				idle.Reset(request.IdleTimeout)
				sampleDue = true
				continue
			default:
			}
			if ctx.Err() != nil {
				err = <-done
				break loop
			}
			before := previous
			current := checkActivity()
			if groupMadeProgress(before, current) {
				continue
			}
			idleCondition = "inactivity_deadline"
			idleSnapshot = newIdleSnapshot(current, baseline)
			cancel()
			err = <-done
			break loop
		case <-ctx.Done():
			err = <-done
			break loop
		}
	}
	if partial != nil {
		partial.flush()
	}
	result := collectProcessResult(out, stderr, cmd, startedAt)
	return processOutcome(parent, ctx, result, idleCondition, idleSnapshot, err)
}

// processCommand prepares the command in its own process group, with a clean
// environment and the prompt on standard input.
func processCommand(ctx context.Context, request ProcessRequest, out io.Writer, stderr *capture) *exec.Cmd {
	cmd := exec.CommandContext(ctx, request.Executable, request.Arguments...)
	cmd.Dir = request.Directory
	cmd.Env = CleanEnvironment()
	cmd.Stdin = strings.NewReader(request.Input)
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

// lastOutputAt is when either stream last wrote, or the start without output.
func lastOutputAt(out, stderr *capture, startedAt time.Time) time.Time {
	out.mu.Lock()
	last := out.last
	out.mu.Unlock()
	stderr.mu.Lock()
	if stderr.last.After(last) {
		last = stderr.last
	}
	stderr.mu.Unlock()
	if last.IsZero() {
		last = startedAt
	}
	return last
}

// collectProcessResult copies the captured output and the exit code of the finished process.
func collectProcessResult(out, stderr *capture, cmd *exec.Cmd, startedAt time.Time) ProcessResult {
	out.mu.Lock()
	data := append([]byte(nil), out.data.Bytes()...)
	truncated := out.overflow
	outBytes, outFirst, outLast := out.total, out.first, out.last
	out.mu.Unlock()
	stderr.mu.Lock()
	diagnostic := append([]byte(nil), stderr.data.Bytes()...)
	truncated = truncated || stderr.overflow
	errBytes := stderr.total
	stderr.mu.Unlock()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	offset := func(t time.Time) time.Duration {
		if t.IsZero() {
			return -1
		}
		return t.Sub(startedAt)
	}
	result := ProcessResult{Stdout: data, Stderr: diagnostic, Code: code, Truncated: truncated,
		Duration: time.Since(startedAt), FirstOutput: offset(outFirst), LastOutput: offset(outLast), StdoutBytes: outBytes, StderrBytes: errBytes}
	return result
}

// processOutcome names why the process ended and turns an abnormal end into an error.
func processOutcome(parent, ctx context.Context, result ProcessResult, idleCondition string, idleSnapshot *IdleSnapshot, err error) (ProcessResult, error) {
	truncated := result.Truncated
	if truncated {
		result.Cause = "output_overflow"
		return result, errors.New("AIの出力が上限を超えました")
	}
	if parent.Err() != nil {
		result.Cause = "canceled_by_server"
		return result, parent.Err()
	}
	if ctx.Err() != nil {
		// 無出力が続いた時間切れ（IdleTimeout）と、全体の制限時間（Timeout）を区別する。
		result.Cause = "timeout_total"
		if idleCondition != "" {
			result.Cause, result.Idle, result.IdleCondition = "timeout_idle", idleSnapshot, idleCondition
		}
		return result, failed("timeout", "AIの制限時間または応答待ち時間を超えました", true)
	}
	if err != nil {
		result.Cause = "exit_nonzero"
		return result, errors.New("AIが正常な回答を返さず終了しました")
	}
	return result, nil
}

var _ io.Writer = (*capture)(nil)
