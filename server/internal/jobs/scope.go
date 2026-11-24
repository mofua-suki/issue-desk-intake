package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"issue-desk-intake/server/internal/contract"
)

func normalizePath(raw string) (string, error) {
	value := strings.ReplaceAll(strings.TrimSpace(norm.NFC.String(raw)), "\\", "/")
	if value == "" || contract.UTF16Length(value) > 200 || strings.HasPrefix(value, "/") {
		return "", errors.New("invalid scope path")
	}
	for _, r := range value {
		if r < 32 || r == 127 || strings.ContainsRune("*?[]{}<>|\"`:／＼", r) {
			return "", errors.New("invalid scope path")
		}
	}
	directory := strings.HasSuffix(value, "/")
	parts := []string{}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." {
			continue
		}
		upper := strings.Map(unicode.ToUpper, part)
		base := strings.TrimRight(strings.Split(upper, ".")[0], " ")
		if part == ".." || upper == ".GIT" || strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") || oneOf(base, "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$") || (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && len([]rune(base)) == 4 && strings.ContainsRune("0123456789¹²³", []rune(base)[3]) {
			return "", errors.New("invalid scope path")
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", errors.New("invalid scope path")
	}
	value = strings.Join(parts, "/")
	if directory {
		value += "/"
	}
	return value, nil
}

func ScopeOverlap(a, b, exempt []string) bool {
	ignore := map[string]bool{}
	for _, value := range exempt {
		ignore[strings.Map(unicode.ToUpper, value)] = true
	}
	for _, x := range a {
		upperX := strings.Map(unicode.ToUpper, x)
		if ignore[upperX] || IsTestScopePath(x) {
			continue
		}
		for _, y := range b {
			upperY := strings.Map(unicode.ToUpper, y)
			if ignore[upperY] || IsTestScopePath(y) {
				continue
			}
			if x == "*" || y == "*" || upperX == upperY || strings.HasSuffix(x, "/") && strings.HasPrefix(upperY, upperX) || strings.HasSuffix(y, "/") && strings.HasPrefix(upperX, upperY) {
				return true
			}
		}
	}
	return false
}

func NormalizeScope(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, contract.NewProblem(400, "addは空でないパスの配列です。", "")
	}
	values := []string{}
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "*" {
			values = append(values, "*")
			continue
		}
		normalized, err := normalizePath(path)
		if err != nil {
			return nil, contract.NewProblem(400, "範囲に読めないパスがあります。", "")
		}
		if !seen[normalized] {
			values = append(values, normalized)
			seen[normalized] = true
		}
	}
	if oneOf("*", values...) {
		return []string{"*"}, nil
	}
	if len(paths) <= 200 {
		return values, nil
	}
	return collapseScope(values), nil
}

// collapseScope は200件を超える範囲を、親のディレクトリへまとめて200件以下にする。まとめても減らなければ * にする。
func collapseScope(values []string) []string {
	parent := func(value string) string {
		parts := strings.Split(strings.TrimSuffix(value, "/"), "/")
		if len(parts) > 1 {
			return strings.Join(parts[:len(parts)-1], "/") + "/"
		}
		return value
	}
	drop := func(input []string) []string {
		out := []string{}
		seen := map[string]bool{}
		for _, path := range input {
			if seen[path] {
				continue
			}
			seen[path] = true
			covered := false
			for _, candidate := range input {
				if candidate != path && strings.HasSuffix(candidate, "/") && strings.HasPrefix(path, candidate) {
					covered = true
					break
				}
			}
			if !covered {
				out = append(out, path)
			}
		}
		return out
	}
	for i, value := range values {
		if !strings.HasSuffix(value, "/") {
			values[i] = parent(value)
		}
	}
	values = drop(values)
	for len(values) > 200 {
		next := make([]string, len(values))
		for i, value := range values {
			next[i] = parent(value)
		}
		next = drop(next)
		if equivalent(next, values) {
			return []string{"*"}
		}
		values = next
	}
	return values
}

type Blocker struct {
	ID    int64   `json:"id"`
	Repo  string  `json:"repo"`
	Issue int64   `json:"issue"`
	Host  *string `json:"host"`
}
type ScopeResult struct {
	Result   string    `json:"result"`
	Job      *Job      `json:"job"`
	Blockers []Blocker `json:"blockers,omitempty"`
}

func (s *Store) Widen(ctx context.Context, id int64, host string, epoch int64, paths, exempt []string, files ...int) (*ScopeResult, error) {
	extra, err := NormalizeScope(paths)
	if err != nil {
		return nil, err
	}
	at := s.options.Now()
	var result *ScopeResult
	overLimit := false
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		job, err := fenced(ctx, tx, id, host, epoch, at, false)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", jobsAdvisoryLockKey(job.Repo)); err != nil {
			return err
		}
		limit, err := measureScopeLimit(ctx, tx, job, paths, extra, files)
		if err != nil {
			return err
		}
		if limit.over() && !limit.approved {
			if err := stopScopeOverLimit(ctx, tx, job, host, scopeRequestedPaths(paths), limit.count, limit.limit, limit.baseline, limit.uncounted, at); err != nil {
				return err
			}
			overLimit = true
			return nil
		}
		if limit.over() && limit.approved {
			if err := recordScopeOverLimitApproval(ctx, tx, job, host, paths, limit.count, at); err != nil {
				return err
			}
		}
		blockers, err := scopeBlockers(ctx, tx, job, extra, exempt)
		if err != nil {
			return err
		}
		merged := mergeScope(job.Scope, extra)
		wait := len(blockers) > 0
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET scope=$2::text[],scope_source='widened',status=CASE WHEN $3 THEN 'held' ELSE status END,hold_reason=CASE WHEN $3 THEN 'scope_wait' ELSE hold_reason END,lease_expires_at=CASE WHEN $3 THEN NULL ELSE lease_expires_at END,updated_at=$4 WHERE id=$1`, id, merged, wait, at)
		if err != nil {
			return err
		}
		if wait {
			if _, err = tx.ExecContext(ctx, "UPDATE merge_turns SET holder_host=NULL,job_id=NULL,expires_at=NULL WHERE job_id=$1", id); err != nil {
				return err
			}
		}
		kind := "scope_widened"
		outcome := "granted"
		data := map[string]any{"add": extra, "blockers": blockers, "baseline": limit.baseline, "uncounted": limit.uncounted}
		if wait {
			kind, outcome = "scope_wait", "wait"
			data["to"] = "held"
			data["leaseHost"] = job.LeaseHost
			data["generation"] = job.Generation
			data["epoch"] = job.LeaseEpoch
			if phase, ok := job.View["phase"]; ok {
				data["phase"] = phase
			}
		}
		if err = addEvent(ctx, tx, id, host, kind, data, at); err != nil {
			return err
		}
		job, err = readJob(ctx, tx, id, false)
		if err != nil {
			return err
		}
		result = &ScopeResult{Result: outcome, Job: job}
		if wait {
			result.Blockers = blockers
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Returning this from the transaction callback would roll back the stop.
	if overLimit {
		return nil, contract.NewProblem(409, "変更範囲が目安を超えたため停止しました。このジョブの lease はもうありません。", "lease_lost")
	}
	return result, err
}

// scopeLimit は広げた後の範囲のファイル数と、その上限。
type scopeLimit struct {
	baseline, count, limit int
	uncounted              []string
	approved               bool
}

// over は広げた後のファイル数が上限を超えるかを返す。
func (l scopeLimit) over() bool { return l.count > l.limit }

// measureScopeLimit は広げた後のファイル数と、最初の範囲・指定の下限から決まる上限、上限超えの承認の有無を求める。
func measureScopeLimit(ctx context.Context, tx *sql.Tx, job *Job, paths, extra []string, files []int) (scopeLimit, error) {
	baseline, approved, err := scopeLimitHistory(ctx, tx, job)
	if err != nil {
		return scopeLimit{}, err
	}
	// Count each requested path before NormalizeScope can collapse a wildcard or
	// a large request; otherwise concrete files could disappear from the count.
	count, _ := scopeFileCount(append(append([]string{}, job.Scope...), paths...))
	// Keep original unspecified entries even if NormalizeScope collapses them to *.
	_, uncounted := scopeFileCount(append(append(append([]string{}, job.Scope...), extra...), paths...))
	floor := scopeFileLimit
	if len(files) > 0 && files[0] > 0 && int64(files[0]) <= maxSafeInteger {
		floor = files[0]
	}
	return scopeLimit{baseline: baseline, count: count, limit: max(floor, 2*baseline), uncounted: uncounted, approved: approved}, nil
}

// recordScopeOverLimitApproval は承認済みの上限超えを、依頼ごとに1回だけ記録する。
func recordScopeOverLimitApproval(ctx context.Context, tx *sql.Tx, job *Job, host string, paths []string, count int, at time.Time) error {
	var recorded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='scope_over_limit_approved')`, job.ID).Scan(&recorded); err != nil {
		return err
	}
	if !recorded {
		if err := addEvent(ctx, tx, job.ID, host, "scope_over_limit_approved", map[string]any{"count": count, "add": scopeRequestedPaths(paths)}, at); err != nil {
			return err
		}
	}
	return nil
}

// scopeBlockers は同じリポジトリで実行中のほかの依頼のうち、広げる範囲と重なるものを集める。
func scopeBlockers(ctx context.Context, tx *sql.Tx, job *Job, extra, exempt []string) ([]Blocker, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE status='running' AND lower(repo)=lower($1) AND id<>$2", job.Repo, job.ID)
	if err != nil {
		return nil, err
	}
	blockers := []Blocker{}
	for rows.Next() {
		busy, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if ScopeOverlap(extra, busy.Scope, exempt) {
			blockers = append(blockers, Blocker{ID: busy.ID, Repo: busy.Repo, Issue: busy.Issue, Host: busy.LeaseHost})
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return blockers, nil
}

// mergeScope は今の範囲に広げる範囲を重複なく足す。* を含むなら * だけにする。
func mergeScope(scope, extra []string) []string {
	merged := append([]string{}, scope...)
	seen := map[string]bool{}
	for _, path := range merged {
		seen[path] = true
	}
	for _, path := range extra {
		if !seen[path] {
			merged = append(merged, path)
			seen[path] = true
		}
	}
	if seen["*"] {
		merged = []string{"*"}
	}
	return merged
}

func (s *Store) Event(ctx context.Context, id int64, host string, epoch int64, kind string, data map[string]any) error {
	if !oneOf(kind, "step_start", "step_end", "failure", "info", "handed_back") {
		return contract.NewProblem(400, "出来事の種類が不正です。", "")
	}
	if err := checkObject(data, "data"); err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	at := s.options.Now()
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := fenced(ctx, tx, id, host, epoch, at, true); err != nil {
			return err
		}
		return addEvent(ctx, tx, id, host, kind, data, at)
	})
}

func (s *Store) Note(ctx context.Context, id int64, host string, epoch int64, kind string, data map[string]any) (int64, error) {
	if !oneOf(kind, "finding", "followup", "improvement") || data == nil {
		return 0, contract.NewProblem(400, "注意点の種類とdataが必要です。", "")
	}
	if err := checkObject(data, "data"); err != nil {
		return 0, err
	}
	at := s.options.Now()
	var noteID int64
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		job, err := fenced(ctx, tx, id, host, epoch, at, true)
		if err != nil {
			return err
		}
		raw, err := jsonValue(data)
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "INSERT INTO repo_notes(repo,kind,job_id,at,data) VALUES($1,$2,$3,$4,$5::jsonb) RETURNING id", job.Repo, kind, id, at, raw).Scan(&noteID)
	})
	return noteID, err
}
