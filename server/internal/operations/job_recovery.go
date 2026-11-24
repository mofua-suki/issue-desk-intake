package operations

// Job recovery stores its decisions independently of incident monitoring. It does
// not schedule work or send commands.
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

type RecoveryCandidate struct {
	Job   jobs.Job
	Cause string
}
type RecoveryCause struct {
	RequiresUser  *bool     `json:"requiresUser,omitempty"`
	Handoff       bool      `json:"handoff,omitempty"`
	CauseHash     string    `json:"causeHash"`
	Generation    int64     `json:"generation"`
	State         string    `json:"state"`
	ReservationID string    `json:"reservationId"`
	At            time.Time `json:"at"`
	PreviousCause string    `json:"previousCause,omitempty"`
	Notified      bool      `json:"notified"`
	SendCount     int       `json:"sendCount"`
	PreSendErrors int       `json:"preSendErrors,omitempty"`
	LastResult    string    `json:"lastResult,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Evidence      string    `json:"evidence,omitempty"`
	Action        string    `json:"action,omitempty"`
	ActionText    string    `json:"actionText,omitempty"`
	Abandoned     bool      `json:"abandoned"`
}
type RecoveryRecord struct {
	Repo            string                   `json:"repo"`
	Issue           int64                    `json:"issue"`
	Cause           string                   `json:"cause"`
	Causes          map[string]RecoveryCause `json:"causes"`
	ExclusionReason string                   `json:"exclusionReason,omitempty"`
	ExclusionAt     time.Time                `json:"exclusionAt"`
}
type JobRecovery struct {
	Service            *Service
	ReservationTimeout time.Duration
	PreSendErrorLimit  int
	MinAge             time.Duration
	Limit              int
	RunLimit           int64
	ReviewLimit        int64
	UsageLimit         float64
	UsageWeights       map[string]any
}

func recoveryCause(j *jobs.Job) string {
	var v any
	if j.Status == "held" && j.HoldReason != nil && *j.HoldReason == "question" {
		v = j.Result["questions"]
		if recoveryFollowupWithoutQuestion(j) {
			v = []any{j.Result["incomplete"], j.Result["summary"]}
		}
	} else if j.Status == "held" && j.HoldReason != nil && *j.HoldReason == "review_held" {
		review, ok := j.Result["review"].(map[string]any)
		if !ok {
			review, _ = j.Result["previousReview"].(map[string]any)
		}
		v = []any{j.Result["summary"], review}
	} else {
		v = j.Result["error"]
		if strings.TrimSpace(fleet.Text(v)) == "" {
			v = j.Result["reason"]
		}
	}
	var parts []string
	var visit func(any)
	visit = func(x any) {
		switch a := x.(type) {
		case string:
			if strings.TrimSpace(a) != "" {
				parts = append(parts, a)
			}
		case []any:
			for _, z := range a {
				visit(z)
			}
		case map[string]any:
			keys := make([]string, 0, len(a))
			for k := range a {
				// Ignore review classification metadata but include all body fields,
				// including nested and future finding formats.
				switch k {
				case "decision", "status", "kind", "severity", "findingKinds", "findingSeverities", "previous", "breaking", "tooLarge", "omitted", "head", "model", "provider", "at", "createdAt", "updatedAt", "timestamp":
					continue
				}
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				visit(a[k])
			}
		}
	}
	visit(v)
	normalized := incidentCause(map[string]any{"error": strings.Join(parts, " ")})
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

const recoveryReservationTimeout = time.Hour

func (r *JobRecovery) ReservationTTL() time.Duration {
	if r.ReservationTimeout > 0 {
		return r.ReservationTimeout
	}
	return recoveryReservationTimeout
}

func (r *JobRecovery) preSendErrorLimit() int {
	if r.PreSendErrorLimit > 0 {
		return r.PreSendErrorLimit
	}
	return 3
}

var errRecoveryLedgerFull = errors.New("job recovery ledger full")
var errRecoveryUnchanged = errors.New("job recovery unchanged")

func (r *JobRecovery) ledger(ctx context.Context, change func(*[]RecoveryRecord) error) error {
	if r.Service == nil || r.Service.PostgreSQL == nil {
		return fmt.Errorf("job recovery database unavailable")
	}
	tx, e := r.Service.PostgreSQL.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('issue-desk-job-recovery'))"); e != nil {
		return e
	}
	rows, e := loadRecoveryLedger(ctx, tx, r.Service.now())
	if e != nil {
		return e
	}
	if e = change(&rows); e != nil {
		if errors.Is(e, errRecoveryUnchanged) {
			return nil
		}
		return e
	}
	if rows, e = evictRecoveryRecords(ctx, tx, rows); e != nil {
		return e
	}
	b, e := json.Marshal(rows)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO settings(key,value,expires_at,updated_at,updated_by) VALUES('job-recovery',$1::jsonb,NULL,$2,'job-recovery') ON CONFLICT(key) DO UPDATE SET value=excluded.value,expires_at=NULL,updated_at=excluded.updated_at,updated_by=excluded.updated_by", string(b), r.Service.now())
	if e != nil {
		return e
	}
	return tx.Commit()
}

// loadRecoveryLedger は自動復旧の台帳を行ロック付きで読む。台帳が無ければ空を返す。
func loadRecoveryLedger(ctx context.Context, tx *sql.Tx, now time.Time) ([]RecoveryRecord, error) {
	var raw string
	rows := []RecoveryRecord{}
	e := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='job-recovery' AND (expires_at IS NULL OR expires_at>$1) FOR UPDATE", now).Scan(&raw)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	if e == nil {
		if e = json.Unmarshal([]byte(raw), &rows); e != nil {
			return nil, e
		}
		if rows == nil {
			return nil, fmt.Errorf("invalid recovery ledger")
		}
	}
	return rows, nil
}

// evictRecoveryRecords は台帳が200件を超えたとき、閉じたジョブの記録を古い順に外す。外せる記録が無ければ台帳が満杯として断る。
func evictRecoveryRecords(ctx context.Context, tx *sql.Tx, rows []RecoveryRecord) ([]RecoveryRecord, error) {
	// Only closed jobs are eligible for eviction; active reservations are never removed.
	for len(rows) > 200 {
		idx := -1
		var oldest time.Time
		for i, rec := range rows {
			reserved := false
			var at time.Time
			for _, c := range rec.Causes {
				if c.State == "reserved" {
					reserved = true
				}
				if c.At.After(at) {
					at = c.At
				}
			}
			if reserved {
				continue
			}
			var closed bool
			if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE repo=$1 AND issue=$2 AND status IN ('closed','merged'))", rec.Repo, rec.Issue).Scan(&closed); e != nil {
				return nil, e
			}
			if closed && (idx < 0 || at.Before(oldest)) {
				idx = i
				oldest = at
			}
		}
		if idx < 0 {
			return nil, errRecoveryLedgerFull
		}
		rows = append(rows[:idx], rows[idx+1:]...)
	}
	return rows, nil
}

func (r *JobRecovery) records(ctx context.Context) ([]RecoveryRecord, error) {
	v, e := r.Service.OperationalSetting(ctx, "job-recovery")
	if e != nil {
		return nil, e
	}
	if v == nil {
		return nil, nil
	}
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var rows []RecoveryRecord
	e = json.Unmarshal(b, &rows)
	return rows, e
}
func (r *JobRecovery) Reserve(ctx context.Context, c RecoveryCandidate) (string, string, error) {
	result, id := "", ""
	if c.Cause == "" {
		return "", "", fmt.Errorf("empty cause")
	}
	e := r.ledger(ctx, func(rows *[]RecoveryRecord) error {
		row := findOrAddRecoveryRow(rows, c)
		// An unfinished send cannot be replaced, even when the job changes.
		if r.expireRecoveryReservations(row) {
			result = "already_reserved"
			return nil
		}
		old, exists := row.Causes[c.Cause]
		if exists && (old.State == "abandoned" || (old.State != "pre_send_error" && old.Generation >= c.Job.Generation)) {
			result = "already_reserved"
			return nil
		}
		if exists && old.Generation == c.Job.Generation && old.State == "pre_send_error" && old.PreSendErrors >= r.preSendErrorLimit() {
			old.State = "skipped"
			old.LastResult = "pre_send_error_limit"
			row.Causes[c.Cause] = old
			result = "already_reserved"
			return nil
		}
		previous := row.Cause
		if previous == c.Cause {
			previous = old.PreviousCause
		}
		if exists && (old.State == "sent" || old.State == "send_unknown" || old.State == "send_rejected" || old.Handoff && recoveryFollowup(&c.Job)) {
			old.CauseHash = c.Cause
			old.State = "abandoned"
			old.Abandoned = true
			old.Generation = c.Job.Generation
			old.At = r.Service.now()
			old.PreviousCause = previous
			if recoveryFollowup(&c.Job) {
				old.Handoff = true
				old.Reason = "同じ原因が再発しました。受付担当が元の指摘、質問、未完了理由と不足資料を照合してください。"
				old.ActionText = recoveryFollowupHandoffReport(&c.Job)
			}
			row.Causes[c.Cause] = old
			row.Cause = c.Cause
			result = "abandon"
			return nil
		}
		// The ID is stable across processes and is accepted by the command API.
		id = recoveryReservationID(c)
		errors := 0
		if exists && old.Generation == c.Job.Generation {
			errors = old.PreSendErrors
		}
		row.Causes[c.Cause] = RecoveryCause{PreSendErrors: errors, CauseHash: c.Cause, Generation: c.Job.Generation, State: "reserved", ReservationID: id, At: r.Service.now(), PreviousCause: previous}
		row.Cause = c.Cause
		result = "proceed"
		return nil
	})
	return result, id, e
}

// findOrAddRecoveryRow は台帳から依頼の記録を探し、無ければ足して返す。
func findOrAddRecoveryRow(rows *[]RecoveryRecord, c RecoveryCandidate) *RecoveryRecord {
	var row *RecoveryRecord
	for i := range *rows {
		if (*rows)[i].Repo == c.Job.Repo && (*rows)[i].Issue == c.Job.Issue {
			row = &(*rows)[i]
			break
		}
	}
	if row == nil {
		*rows = append(*rows, RecoveryRecord{Repo: c.Job.Repo, Issue: c.Job.Issue, Causes: map[string]RecoveryCause{}})
		row = &(*rows)[len(*rows)-1]
	}
	if row.Causes == nil {
		row.Causes = map[string]RecoveryCause{}
	}
	return row
}

// expireRecoveryReservations は期限を過ぎた送信予約を結果不明にする。期限内の予約が残っていれば true を返す。
func (r *JobRecovery) expireRecoveryReservations(row *RecoveryRecord) bool {
	for hash, active := range row.Causes {
		if active.State != "reserved" {
			continue
		}
		if active.At.IsZero() || !active.At.Add(r.ReservationTTL()).After(r.Service.now()) {
			active.State, active.LastResult = "send_unknown", "send_unknown"
			active.SendCount++
			active.At = r.Service.now()
			row.Causes[hash] = active
			continue
		}
		return true
	}
	return false
}

// recoveryReservationID は依頼・世代・原因から、プロセスをまたいで変わらない送信IDを作る。
func recoveryReservationID(c RecoveryCandidate) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("job-recovery:%d:%d:%s", c.Job.ID, c.Job.Generation, c.Cause)))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (r *JobRecovery) Finish(ctx context.Context, c RecoveryCandidate, id, state string) error {
	if state != "pre_send_error" && state != "skipped" && state != "sent" && state != "send_unknown" && state != "send_rejected" {
		return fmt.Errorf("invalid recovery result")
	}
	return r.ledger(ctx, func(rows *[]RecoveryRecord) error {
		for i := range *rows {
			row := &(*rows)[i]
			if row.Repo == c.Job.Repo && row.Issue == c.Job.Issue {
				old, ok := row.Causes[c.Cause]
				if !ok || old.State != "reserved" || old.ReservationID != id {
					return fmt.Errorf("reservation mismatch")
				}
				old.State = state
				if state == "pre_send_error" {
					old.PreSendErrors++
				}
				old.LastResult = state
				if state == "sent" || state == "send_unknown" || state == "send_rejected" {
					old.SendCount++
				}
				old.At = r.Service.now()
				row.Causes[c.Cause] = old
				return nil
			}
		}
		return fmt.Errorf("reservation missing")
	})
}

func (r *JobRecovery) Candidates(ctx context.Context) ([]RecoveryCandidate, error) {
	if r.Service == nil || r.Service.PostgreSQL == nil {
		return nil, fmt.Errorf("job recovery database unavailable")
	}
	age := r.MinAge
	if age <= 0 {
		age = 10 * time.Minute
	}
	limit := r.Limit
	if limit <= 0 {
		limit = 3
	}
	runLimit := r.RunLimit
	if runLimit <= 0 {
		runLimit = 4
	}
	configured, waits, abandoned, e := r.recoveryContext(ctx)
	if e != nil {
		return nil, e
	}
	all, e := r.loadRecoveryJobs(ctx, age)
	if e != nil {
		return nil, e
	}
	records, e := r.records(ctx)
	if e != nil {
		return nil, e
	}
	out := []RecoveryCandidate{}
	for _, j := range all {
		if !configured[j.Repo] || jobs.PrerequisiteMissingHeld(&j) {
			continue
		}
		cause := recoveryCause(&j)
		if cause == "" {
			if e = r.recordMissingCause(ctx, records, &j); e != nil && !errors.Is(e, errRecoveryLedgerFull) {
				return nil, e
			}
			continue
		}
		if recoveryAlreadyConsidered(records, RecoveryCandidate{Job: j, Cause: cause}, r.Service.now(), r.ReservationTTL()) {
			continue
		}
		excluded, err := r.recoveryExcluded(ctx, &j, cause, configured, waits, abandoned, runLimit)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		out = append(out, RecoveryCandidate{Job: j, Cause: cause})
	}
	return recoveryOrderCandidates(records, out, limit, r.Service.now()), nil
}

// loadRecoveryJobs は age より前から失敗・保留で止まっているジョブを読む。表示データを読めないジョブは飛ばす。
func (r *JobRecovery) loadRecoveryJobs(ctx context.Context, age time.Duration) ([]jobs.Job, error) {
	rows, e := r.Service.PostgreSQL.QueryContext(ctx, `SELECT id,repo,issue,title,kind,status,hold_reason,generation,lease_host,view,result FROM jobs WHERE (status='failed' OR (status='held' AND hold_reason IN ('review_held','question'))) AND updated_at <= $1 ORDER BY id`, r.Service.now().Add(-age))
	if e != nil {
		return nil, e
	}
	var all []jobs.Job
	for rows.Next() {
		var j jobs.Job
		var view, result []byte
		if e = rows.Scan(&j.ID, &j.Repo, &j.Issue, &j.Title, &j.Kind, &j.Status, &j.HoldReason, &j.Generation, &j.LeaseHost, &view, &result); e != nil {
			rows.Close()
			return nil, e
		}
		if e = json.Unmarshal(view, &j.View); e != nil {
			continue
		}
		j.Result = jobs.StoredResult(j.ID, result)
		all = append(all, j)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	return all, nil
}

func (r *JobRecovery) Recheck(ctx context.Context, c RecoveryCandidate) (bool, error) {
	if c.Cause == "" {
		return false, nil
	}
	var status string
	var hold *string
	var generation int64
	var view, result []byte
	var host *string
	var kind string
	e := r.Service.PostgreSQL.QueryRowContext(ctx, `SELECT status,hold_reason,generation,lease_host,view,result,kind FROM jobs WHERE id=$1 AND repo=$2 AND issue=$3`, c.Job.ID, c.Job.Repo, c.Job.Issue).Scan(&status, &hold, &generation, &host, &view, &result, &kind)
	if e == sql.ErrNoRows {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	j := jobs.Job{ID: c.Job.ID, Repo: c.Job.Repo, Issue: c.Job.Issue, Kind: kind, LeaseHost: host, Status: status, HoldReason: hold, Generation: generation}
	if e = json.Unmarshal(view, &j.View); e != nil {
		return false, e
	}
	j.Result = jobs.StoredResult(c.Job.ID, result)
	if status != c.Job.Status || generation != c.Job.Generation || (hold == nil) != (c.Job.HoldReason == nil) || hold != nil && *hold != *c.Job.HoldReason || recoveryUncertainView(j.View) || jobs.PrerequisiteMissingHeld(&j) || recoveryCause(&j) != c.Cause {
		return false, nil
	}
	configured, waits, abandoned, err := r.recoveryContext(ctx)
	if err != nil {
		return false, err
	}
	runLimit := r.RunLimit
	if runLimit <= 0 {
		runLimit = 4
	}
	excluded, err := r.recoveryExcluded(ctx, &j, c.Cause, configured, waits, abandoned, runLimit)
	return !excluded, err
}
