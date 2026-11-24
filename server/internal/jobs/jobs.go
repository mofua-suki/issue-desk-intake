// Package jobs stores fenced job updates and repository merge turns.
package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"time"

	"issue-desk-intake/server/internal/contract"
)

const maxSafeInteger int64 = 9007199254740991

type Job struct {
	ID             int64  `json:"id"`
	Repo           string `json:"repo"`
	Issue          int64  `json:"issue"`
	Title          string `json:"title"`
	Kind           string `json:"kind"`
	Priority       int    `json:"priority"`
	filedAt        time.Time
	FiledAt        string         `json:"filedAt"`
	Scope          []string       `json:"scope"`
	ScopeSource    string         `json:"scopeSource"`
	Provider       *string        `json:"provider"`
	Tier           *string        `json:"tier"`
	Status         string         `json:"status"`
	HoldReason     *string        `json:"holdReason"`
	Generation     int64          `json:"generation"`
	Branch         *string        `json:"branch"`
	PR             *int64         `json:"pr"`
	HeadSHA        *string        `json:"headSha"`
	LeaseHost      *string        `json:"leaseHost"`
	LeaseEpoch     int64          `json:"leaseEpoch"`
	LeaseExpiresAt *string        `json:"leaseExpiresAt"`
	ProgressAt     *string        `json:"progressAt"`
	View           map[string]any `json:"view"`
	ReviewHolds    any            `json:"reviewHolds"`
	Result         map[string]any `json:"result"`
	UpdatedAt      string         `json:"updatedAt"`
}

type Lease struct {
	Epoch     int64  `json:"epoch"`
	ExpiresAt string `json:"expiresAt"`
}

type Options struct {
	// Publish receives one issue identifier per changed issue, after commit.
	// It is optional and must not block on network I/O.
	Publish       func(map[string]any)
	AppDB         *sql.DB
	ReadIssue     func(context.Context, string, int64) (map[string]any, error)
	ReadPull      func(context.Context, string, int64) (map[string]any, error)
	ReadComments  func(context.Context, string, int64) ([]map[string]any, error)
	ReportAuthor  func(string) string
	Now           func() time.Time
	LeaseDuration time.Duration
	TurnDuration  time.Duration
	WaitDuration  time.Duration
	DrainDuration time.Duration
	// OnMerged participates in the same transaction as the final state report.
	OnMerged func(context.Context, *sql.Tx, *Job, map[string]any) error
}

type Store struct {
	db      *sql.DB
	options Options
	// issueList is nil in a store that New did not make; AllForIssues then decodes every row.
	issueList *issueListCache
}

func New(db *sql.DB, options Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("jobs require a PostgreSQL connection")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = 15 * time.Minute
	}
	if options.TurnDuration == 0 {
		options.TurnDuration = 15 * time.Minute
	}
	if options.WaitDuration == 0 {
		options.WaitDuration = 3 * time.Minute
	}
	if options.DrainDuration == 0 {
		minutes := 150
		if value, exists := os.LookupEnv("ISSUE_DESK_DRAIN_DEFAULT_MINUTES"); exists {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 1440 {
				return nil, errors.New("invalid drain duration")
			}
			minutes = parsed
		}
		options.DrainDuration = time.Duration(minutes) * time.Minute
	}
	if options.LeaseDuration <= 0 || options.TurnDuration <= 0 || options.WaitDuration <= 0 || options.DrainDuration <= 0 || options.DrainDuration > 24*time.Hour {
		return nil, errors.New("job durations must be positive")
	}
	return &Store{db: db, options: options, issueList: newIssueListCache()}, nil
}

func iso(at time.Time) string { return contract.ISO(at) }

func nullableISO(at sql.NullTime) *string {
	if !at.Valid {
		return nil
	}
	value := iso(at.Time)
	return &value
}

const jobColumns = `id,repo,issue,title,kind,priority,filed_at,to_json(scope),scope_source,provider,tier,status,hold_reason,generation,branch,pr,head_sha,lease_host,lease_epoch,lease_expires_at,progress_at,view,result,updated_at`

type rowScanner interface{ Scan(...any) error }
type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func scanJob(row rowScanner) (*Job, error) {
	job, _, err := scanJobSized(row)
	return job, err
}

// scanJobSized also returns the UTF-8 bytes of the repo, title and JSON columns it read.
func scanJobSized(row rowScanner) (*Job, int, error) {
	job := &Job{}
	var filed, updated time.Time
	var expires, progress sql.NullTime
	var scope, view, result []byte
	err := row.Scan(&job.ID, &job.Repo, &job.Issue, &job.Title, &job.Kind, &job.Priority, &filed, &scope, &job.ScopeSource, &job.Provider, &job.Tier, &job.Status, &job.HoldReason, &job.Generation, &job.Branch, &job.PR, &job.HeadSHA, &job.LeaseHost, &job.LeaseEpoch, &expires, &progress, &view, &result, &updated)
	if err != nil {
		return nil, 0, err
	}
	if err = json.Unmarshal(scope, &job.Scope); err != nil {
		return nil, 0, err
	}
	if len(view) > 0 {
		if err = json.Unmarshal(view, &job.View); err != nil {
			return nil, 0, err
		}
	}
	if job.View == nil {
		job.View = map[string]any{}
	}
	validateFingerprintTarget(job)
	job.Result = StoredResult(job.ID, result)
	job.filedAt = filed
	job.FiledAt, job.UpdatedAt = iso(filed), iso(updated)
	job.LeaseExpiresAt, job.ProgressAt = nullableISO(expires), nullableISO(progress)
	job.ReviewHolds = reportedHolds(job.View)
	restorePrerequisiteReason(job)
	return job, len(job.Repo) + len(job.Title) + len(scope) + len(view) + len(result), nil
}

func readJob(ctx context.Context, q queryRower, id int64, lock bool) (*Job, error) {
	query := "SELECT " + jobColumns + " FROM jobs WHERE id=$1"
	if lock {
		query += " FOR UPDATE"
	}
	job, err := scanJob(q.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, contract.NewProblem(404, "ジョブが見つかりません。", "")
	}
	return job, err
}

func (s *Store) Get(ctx context.Context, id int64) (*Job, error) {
	return readJob(ctx, s.db, id, false)
}

func (s *Store) transaction(ctx context.Context, work func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.options.Publish != nil {
		if _, err = tx.ExecContext(ctx, "SELECT set_config('issue_desk.job_changes', '{}', true)"); err != nil {
			return err
		}
	}
	if err = work(tx); err != nil {
		return err
	}
	changes, err := s.jobChanges(ctx, tx)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, change := range changes {
		s.options.Publish(map[string]any{"type": "issue", "project": change.Project, "number": change.Number})
	}
	return nil
}

func CheckFence(job *Job, host string, epoch int64, at time.Time, finished bool) *contract.Problem {
	if epoch < 1 || epoch > maxSafeInteger {
		return contract.NewProblem(400, "epoch は1以上の整数にしてください。", "")
	}
	live := false
	if job.LeaseExpiresAt != nil {
		expires, err := time.Parse(time.RFC3339Nano, *job.LeaseExpiresAt)
		live = err == nil && expires.After(at)
	}
	if job.LeaseHost == nil || *job.LeaseHost != host || job.LeaseEpoch != epoch || !(job.Status == "running" && live || finished && job.Status != "running") {
		return contract.NewProblem(409, "このジョブの lease はもうありません。", "lease_lost")
	}
	return nil
}

func fenced(ctx context.Context, tx *sql.Tx, id int64, host string, epoch int64, at time.Time, finished bool) (*Job, error) {
	if epoch < 1 || epoch > maxSafeInteger {
		return nil, contract.NewProblem(400, "epoch は1以上の整数にしてください。", "")
	}
	job, err := readJob(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if problem := CheckFence(job, host, epoch, at, finished); problem != nil {
		return nil, problem
	}
	return job, nil
}

// StoredResult reads the jobs.result column. A value that is not a JSON object counts as no result:
// failing here would turn one bad row into a failure of every read of the jobs table, and of every
// PC's job acquisition with it. The check constraint on jobs.result keeps such values from being saved.
func StoredResult(id int64, raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		log.Printf("job %d: stored result is not a JSON object and is ignored: %v", id, err)
		return nil
	}
	return result
}

func jsonValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func addEvent(ctx context.Context, tx *sql.Tx, id int64, host, kind string, data map[string]any, at time.Time) error {
	body, err := jsonValue(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,$2,$3,$4,$5::jsonb)", id, host, kind, at, body)
	return err
}
