package startup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// HandoffLimit reserves 15 seconds of the deployer's HANDOFF_MS=75000 for
// promotion and entry checks. It does not establish the <30s entry-outage goal.
const HandoffLimit = 60 * time.Second

// HandoffError contains only fixed operation names and safe classifications.
// Pending includes failed operations and operations whose completion is unknown.
// After an error the caller must retain the lease and the start fence. Resources
// may already be closed, including when their close reported an error; the caller
// must stop and restart the old process to recover, not resume serving.
// Normal shutdown must not be treated as a successful handoff fallback.
type HandoffError struct {
	Stage, Operation, Class string
	Pending                 []string
	cause                   error
}

func (e *HandoffError) Error() string {
	return fmt.Sprintf("handoff stage=%s operation=%s class=%s pending=%s", e.Stage, e.Operation, e.Class, strings.Join(e.Pending, ","))
}
func (e *HandoffError) Unwrap() error { return e.cause }

type handoffStep struct {
	name string
	run  func(context.Context) error
}

// Handoff is one writer's single, explicit handoff attempt. Construct it once
// and share it with all notification callers and with the HTTP handler owner.
// Neither construction nor Handler installation starts a handoff. Startup calls
// Run only for the dedicated handoff notification, not ordinary shutdown.
type Handoff struct {
	HTTP      *HandoffHTTP
	once      sync.Once
	done      chan struct{}
	err       error
	log       func(string)
	stage1    []handoffStep
	stage2    []handoffStep
	accepted  []handoffStep
	resources handoffStep
	lease     handoffStep
}

// NewHandoff combines the shipped component completion contracts. The App must
// already own its writer lease; its business handler must be wrapped with
// h.HTTP.Handler before any request is served. The shared PostgreSQL handle is
// still owned by Run and may only be closed by that owner after Run returns.
func NewHandoff(app *App, lease HandoffLease) (*Handoff, error) {
	if app == nil || app.Gate == nil || lease == nil {
		return nil, errors.New("交代には稼働中のApp・Gate・書き手ロックが必要です")
	}
	h := &Handoff{HTTP: &HandoffHTTP{}, done: make(chan struct{}), log: app.Log}
	r := &handoffResources{app: app, lease: lease}
	noContext := func(name string, run func()) handoffStep {
		return handoffStep{name, func(context.Context) error { run(); return nil }}
	}
	h.stage1 = []handoffStep{
		// An AI turn alone has no filing ticket. Its post-save Submit is
		// rejected by this fence unless filing/project work was already
		// admitted. A rejected AI turn stays thinking for recovery; a rejected
		// post-save Submit stays auto_ready. Quiesce waits for either to save.
		noContext("start_fence", app.Gate.BeginHandoff),
		noContext("background", app.StopBackground),
	}
	if app.Engine != nil {
		h.stage1 = append(h.stage1, handoffStep{"consultations", app.Engine.Quiesce})
	}
	if app.AIFailures != nil {
		h.stage1 = append(h.stage1, noContext("notifications", app.AIFailures.StopNotifications))
	}
	h.stage2 = []handoffStep{noContext("http_boundary", h.HTTP.Begin)}
	if app.Live != nil {
		h.stage2 = append(h.stage2, noContext("sse", app.Live.Close))
	}
	// HTTP must finish before WaitGroup-based service waits: an admitted
	// handler can still register asynchronous work while it is completing.
	h.accepted = []handoffStep{{"gate_idle", app.Gate.WaitIdle}}
	if app.Projects != nil {
		h.accepted = append(h.accepted, handoffStep{"projects", app.Projects.WaitAccepted})
	}
	if app.Issues != nil {
		h.accepted = append(h.accepted, handoffStep{"issues", app.Issues.WaitAccepted})
	}
	h.resources = handoffStep{"resources", func(context.Context) error { return r.close() }}
	h.lease = noContext("lease", r.release)
	return h, nil
}

// Run shares the first call's context, deadline and terminal result with every
// duplicate call. Later callers cannot restart, extend or cancel that attempt.
func (h *Handoff) Run(parent context.Context) error {
	h.once.Do(func() {
		started := time.Now() // keep monotonic time for the whole attempt
		ctx, cancel := context.WithTimeout(parent, HandoffLimit)
		go func() {
			defer close(h.done)
			defer cancel() // wait contexts only; never the App/accepted-work context
			h.err = h.run(ctx, started)
		}()
	})
	<-h.done
	return h.err
}

func (h *Handoff) run(ctx context.Context, started time.Time) error {
	h.record("stage1", "start", "none", started)
	// Establish the start fence before launching the other stage-one operations.
	// Each wait uses the same deadline; the fence is invoked even if cancelled.
	if err := h.wait(ctx, "stage1", h.stage1[:1]); err != nil {
		return h.failed(err, started)
	}
	if err := h.wait(ctx, "stage1", h.stage1[1:]); err != nil {
		return h.failed(err, started)
	}
	if err := h.wait(ctx, "stage1", []handoffStep{{"gate_idle", h.accepted[0].run}}); err != nil {
		return h.failed(err, started)
	}
	h.record("stage1", "complete", "none", started)
	if err := ctx.Err(); err != nil {
		return h.failed(h.failure("stage2", "http_boundary", err, []string{"http_boundary"}), started)
	}
	h.record("stage2", "start", "none", started)
	// A late completion only writes to its buffered result channel. It cannot
	// advance this coordinator after a failed wait.
	if err := h.wait(ctx, "stage2", h.stage2); err != nil {
		return h.failed(err, started)
	}
	if err := h.wait(ctx, "stage2", []handoffStep{{"http_requests", h.HTTP.Wait}}); err != nil {
		return h.failed(err, started)
	}
	if err := h.wait(ctx, "stage2", h.accepted); err != nil {
		return h.failed(err, started)
	}
	for _, step := range []handoffStep{h.resources, h.lease} {
		if err := ctx.Err(); err != nil {
			return h.failed(h.failure("stage2", step.name, err, []string{step.name}), started)
		}
		if err := h.wait(ctx, "stage2", []handoffStep{step}); err != nil {
			return h.failed(err, started)
		}
	}
	h.record("stage2", "complete", "none", started)
	return nil
}

func (h *Handoff) wait(ctx context.Context, stage string, steps []handoffStep) *HandoffError {
	type result struct {
		index int
		err   error
	}
	results := make(chan result, len(steps))
	complete := make([]bool, len(steps))
	for i, step := range steps {
		go func() { results <- result{i, step.run(ctx)} }()
	}
	pending := func() []string {
		names := []string{}
		for i, step := range steps {
			if !complete[i] {
				names = append(names, step.name)
			}
		}
		return names
	}
	for range steps {
		select {
		case result := <-results:
			if result.err == nil {
				complete[result.index] = true
			}
			if err := ctx.Err(); err != nil {
				return h.failure(stage, steps[result.index].name, err, pending())
			}
			if result.err != nil {
				return h.failure(stage, steps[result.index].name, result.err, pending())
			}
		case <-ctx.Done():
			return h.failure(stage, "wait", ctx.Err(), pending())
		}
	}
	return nil
}

func (h *Handoff) failure(stage, operation string, err error, pending []string) *HandoffError {
	class := "operation_failed"
	var cause error // never retain or expose a component's exception text
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		class, cause = "timeout", context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		class, cause = "cancelled", context.Canceled
	}
	return &HandoffError{Stage: stage, Operation: operation, Class: class, Pending: pending, cause: cause}
}

func (h *Handoff) failed(err *HandoffError, started time.Time) error {
	h.record(err.Stage, "failed", err.Class, started)
	return err
}

func (h *Handoff) record(stage, result, class string, started time.Time) {
	if h.log != nil {
		h.log(fmt.Sprintf("handoff stage=%s result=%s class=%s elapsed_ms=%d", stage, result, class, time.Since(started).Milliseconds()))
	}
}
