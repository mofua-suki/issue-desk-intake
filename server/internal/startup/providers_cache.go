package startup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"issue-desk-intake/server/internal/ai"
)

const providerTTL = 3 * time.Second

// Allow two refreshes after the initial check. Slow providers can otherwise
// expire in alternation forever; freshness must fail rather than return stale
// rows or keep meta (and initial navigation) waiting without a bound.
const providerAttempts = 3

var providerNames = [...]string{"codex", "claude"}
var errProviderChanged = errors.New("provider conditions changed")
var errProviderFreshness = errors.New("AIの認証状態を有効期限内に確認できません。再度お試しください。")

// Keys contain only configuration/environment digests, never credential contents.
// File identity plus update metadata catches atomic replacements and in-place updates.
type providerKey struct {
	config                       *Config
	runner                       *ai.Runner
	execute                      reflect.Value
	digest                       [32]byte
	specified, resolved, runtime string
	workingDir                   string
	executable, directory        os.FileInfo
}

func (k providerKey) same(other providerKey) bool {
	return k.config == other.config && k.runner == other.runner && k.execute == other.execute &&
		k.digest == other.digest && k.specified == other.specified && k.resolved == other.resolved &&
		k.runtime == other.runtime && sameProviderFile(k.executable, other.executable, true) &&
		sameProviderFile(k.directory, other.directory, false)
}

func sameProviderFile(a, b os.FileInfo, updates bool) bool {
	if a == nil || b == nil || !os.SameFile(a, b) {
		return false
	}
	return !updates || a.Size() == b.Size() && a.Mode() == b.Mode() &&
		a.ModTime() == b.ModTime() && reflect.DeepEqual(providerChangeTime(a), providerChangeTime(b))
}

// Do not include access time: executing a CLI can itself update it. Support the
// stat change-time names used by Unix platforms without reading executable data.
func providerChangeTime(info os.FileInfo) any {
	value := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if value.IsValid() && value.Kind() == reflect.Struct {
		for _, name := range []string{"Ctim", "Ctimespec", "Ctime"} {
			field := value.FieldByName(name)
			if field.IsValid() && field.CanInterface() {
				return field.Interface()
			}
		}
	}
	return nil
}

func (a *App) providerSnapshot(provider string) (providerKey, ai.Runner, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	config, runner := a.Config, a.Runner
	if config == nil || runner == nil {
		return providerKey{}, ai.Runner{}, false
	}
	copy := *runner
	specified := copy.Codex
	if provider == "claude" {
		specified = copy.Claude
	}
	if specified == "" {
		specified = provider
	}
	key := providerKey{config: config, runner: runner, execute: reflect.ValueOf(copy.Execute), specified: specified, workingDir: config.RuntimeDir}
	// CleanEnvironment is exactly the environment used by ai.Run. In particular,
	// HOME, CODEX_HOME, CLAUDE_CONFIG_DIR, PATH and TMPDIR affect auth/CLI resolution.
	environment := ai.CleanEnvironment()
	sort.Strings(environment)
	cwd, cwdErr := os.Getwd()
	data, marshalErr := json.Marshal([]any{config, environment, cwd})
	key.digest = sha256.Sum256(data)
	resolved, lookupErr := exec.LookPath(specified)
	absolute, executableAbsErr := filepath.Abs(resolved)
	key.resolved, lookupErr = resolveProviderPath(absolute, lookupErr)
	key.executable, _ = os.Stat(key.resolved)
	var absErr error
	key.runtime, absErr = filepath.Abs(config.RuntimeDir)
	key.runtime, _ = filepath.EvalSymlinks(key.runtime)
	key.directory, _ = os.Stat(key.runtime)
	valid := cwdErr == nil && marshalErr == nil && lookupErr == nil && executableAbsErr == nil && absErr == nil &&
		key.executable != nil && key.executable.Mode().IsRegular() && providerChangeTime(key.executable) != nil &&
		key.directory != nil && key.directory.IsDir() && config.RuntimeDir != ""
	// Status receives the captured Runner and runtime, even if config is reloaded.
	return key, copy, valid
}

func resolveProviderPath(path string, prior error) (string, error) {
	if prior != nil {
		return path, prior
	}
	return filepath.EvalSymlinks(path)
}

type providerFlight struct {
	key      providerKey
	reusable bool
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	state    ai.Availability
	err      error
	expires  time.Time
}

type providerCache struct {
	mu      sync.Mutex
	current [2]*providerFlight
	live    map[*providerFlight]bool
	slots   chan struct{}
	wg      sync.WaitGroup
	closed  bool
	now     func() time.Time // monotonic production clock; deterministic expiry tests
}

func (c *providerCache) time() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func providerRow(state ai.Availability) map[string]any {
	return map[string]any{"provider": state.Provider, "ready": state.Ready, "reason": state.Reason}
}

func (a *App) cachedProviders(ctx context.Context) ([]map[string]any, error) {
	for range providerAttempts {
		rows, err := a.providersAttempt(ctx)
		if !errors.Is(err, errProviderChanged) {
			return rows, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errProviderFreshness
}

func (a *App) providersAttempt(ctx context.Context) ([]map[string]any, error) {
	flights, err := a.joinProviderFlights(ctx)
	if err != nil {
		return nil, err
	}
	defer a.releaseProviderFlights(flights)
	rows, err := awaitProviderRows(ctx, flights)
	if err != nil {
		return nil, err
	}
	if err := a.checkProviderFreshness(ctx, flights); err != nil {
		return nil, err
	}
	return rows, nil
}

// joinProviderFlights は実行先ごとに、再利用できる取得に加わるか、無ければ新しい取得を始め、その待ち手として登録する。
func (a *App) joinProviderFlights(ctx context.Context) ([2]*providerFlight, error) {
	c := &a.providers
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return [2]*providerFlight{}, err
	}
	if c.closed || a.ctx != nil && a.ctx.Err() != nil {
		c.mu.Unlock()
		return [2]*providerFlight{}, context.Canceled
	}
	if c.live == nil {
		c.live = make(map[*providerFlight]bool)
		c.slots = make(chan struct{}, 2)
	}
	flights := [2]*providerFlight{}
	for i, name := range providerNames {
		key, runner, valid := a.providerSnapshot(name)
		f := c.current[i]
		if f == nil || !valid || !f.reusable || !f.key.same(key) || f.ctx.Err() != nil ||
			f.finished && !c.time().Before(f.expires) {
			if f != nil && f.finished && f.waiters == 0 {
				f.cancel()
			}
			parent := a.ctx
			if parent == nil {
				parent = context.Background()
			}
			work, cancel := context.WithCancel(parent)
			f = &providerFlight{key: key, reusable: valid, ctx: work, cancel: cancel, done: make(chan struct{})}
			c.current[i] = f
			c.live[f] = true
			c.wg.Add(1)
			go a.runProvider(i, name, runner, key.workingDir, f)
		}
		f.waiters++
		flights[i] = f
	}
	c.mu.Unlock()
	return flights, nil
}

// releaseProviderFlights は待ち手を外し、待ち手のいなくなった未完了の取得を切り離して止める。
func (a *App) releaseProviderFlights(flights [2]*providerFlight) {
	c := &a.providers
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, f := range flights {
		f.waiters--
		if f.waiters == 0 && !f.finished {
			// Detach before cancelling: a new caller must never join abandoned work.
			if c.current[i] == f {
				c.current[i] = nil
			}
			f.cancel()
		}
		if f.waiters == 0 && f.finished && c.current[i] != f {
			f.cancel()
		}
	}
}

// awaitProviderRows は両方の実行先の取得を待ち、結果を一覧の行にする。
func awaitProviderRows(ctx context.Context, flights [2]*providerFlight) ([]map[string]any, error) {
	rows := make([]map[string]any, 0, 2)
	for _, f := range flights {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.ctx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, f.ctx.Err()
		case <-f.done:
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := f.ctx.Err(); err != nil {
			return nil, err
		}
		if f.err != nil {
			return nil, f.err
		}
		rows = append(rows, providerRow(f.state))
	}
	return rows, nil
}

// checkProviderFreshness は待っている間に先に終わった結果が期限切れになっていないかを、同じ時刻で全体について確かめる。
func (a *App) checkProviderFreshness(ctx context.Context, flights [2]*providerFlight) error {
	c := &a.providers
	// A fast provider can expire while we await the other provider. Validate the
	// whole response after all waits, using one clock reading. The next attempt
	// reuses fresh results/flights and refreshes only expired providers, within
	// cachedProviders' attempt budget.
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.time()
	for _, f := range flights {
		if err := f.ctx.Err(); err != nil {
			return err
		}
		if !now.Before(f.expires) {
			return errProviderChanged
		}
	}
	return nil
}

func (a *App) runProvider(index int, name string, runner ai.Runner, runtime string, f *providerFlight) {
	c := &a.providers
	defer c.wg.Done()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-f.ctx.Done():
	}
	// Recheck after queuing for a slot; cancelled work must not launch a process.
	err := f.ctx.Err()
	key, _, valid := a.providerSnapshot(name)
	if err == nil && f.reusable && (!valid || !f.key.same(key)) {
		err = errProviderChanged
	}
	var state ai.Availability
	if err == nil {
		state = runner.Status(f.ctx, name, runtime)
	}
	completed := time.Now()
	key, _, valid = a.providerSnapshot(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now != nil {
		completed = c.time()
	}
	if f.ctx.Err() != nil {
		err = f.ctx.Err()
	}
	f.state, f.err, f.finished = state, err, true
	f.expires = completed.Add(providerTTL)
	// Only this generation can publish; unknown/changed conditions and cancellation
	// never become cached authentication failures. Existing waiters still get errors.
	if err != nil || !valid || !f.reusable || !f.key.same(key) {
		if c.current[index] == f {
			c.current[index] = nil
		}
	}
	delete(c.live, f)
	close(f.done)
}

func (c *providerCache) close() {
	c.mu.Lock()
	c.closed = true
	for f := range c.live {
		f.cancel()
	}
	for _, f := range c.current {
		if f != nil {
			f.cancel()
		}
	}
	c.current = [2]*providerFlight{}
	c.mu.Unlock()
	c.wg.Wait()
}
