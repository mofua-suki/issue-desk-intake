package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/hosts"
)

type TurnRequest struct {
	Host      string  `json:"host"`
	JobID     *int64  `json:"jobId"`
	RequestID *string `json:"requestId"`
	Epoch     int64   `json:"epoch"`
	TurnEpoch int64   `json:"turnEpoch"`
}
type Turn struct {
	Repo       string  `json:"repo"`
	HolderHost *string `json:"holderHost"`
	JobID      *int64  `json:"jobId"`
	RequestID  *string `json:"requestId"`
	Epoch      int64   `json:"epoch"`
	ExpiresAt  *string `json:"expiresAt"`
	waiters    map[string]waiter
}
type Holder struct {
	Host      string  `json:"host"`
	JobID     *int64  `json:"jobId"`
	RequestID *string `json:"requestId"`
	ExpiresAt string  `json:"expiresAt"`
}
type TurnResult struct {
	Granted bool    `json:"granted"`
	Reason  string  `json:"reason,omitempty"`
	Turn    *Lease  `json:"turn,omitempty"`
	Holder  *Holder `json:"holder,omitempty"`
}
type waiter struct {
	JobID     *int64  `json:"jobId"`
	RequestID *string `json:"requestId"`
	Since     string  `json:"since"`
	At        string  `json:"at"`
}

func validateHolder(who TurnRequest) error {
	if who.Host == "" || (who.JobID == nil) == (who.RequestID == nil) {
		return contract.NewProblem(400, "jobId か requestId のどちらか1つと送り手が必要です。", "")
	}
	if who.JobID != nil && (*who.JobID < 1 || *who.JobID > maxSafeInteger || who.Epoch < 1 || who.Epoch > maxSafeInteger) {
		return contract.NewProblem(400, "jobId と epoch は1以上の整数にしてください。", "")
	}
	if who.RequestID != nil && !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(*who.RequestID) {
		return contract.NewProblem(400, "requestId の形が違います。", "")
	}
	return nil
}

func checkHolder(ctx context.Context, tx *sql.Tx, who TurnRequest, at time.Time) (string, error) {
	if err := validateHolder(who); err != nil {
		return "", err
	}
	if who.JobID != nil {
		job, err := fenced(ctx, tx, *who.JobID, who.Host, who.Epoch, at, false)
		if err != nil {
			return "", err
		}
		return job.Repo, nil
	}
	var repo, status string
	var host *string
	err := tx.QueryRowContext(ctx, "SELECT repo,status,claimed_by FROM requests WHERE id=$1 FOR UPDATE", *who.RequestID).Scan(&repo, &status, &host)
	if errors.Is(err, sql.ErrNoRows) {
		return "", contract.NewProblem(404, "依頼が見つかりません。", "")
	}
	if err != nil {
		return "", err
	}
	if status != "claimed" || host == nil || *host != who.Host {
		return "", contract.NewProblem(409, "この依頼は送り手のPCが引き取っていません。", "turn_lost")
	}
	return repo, nil
}

func readTurn(ctx context.Context, tx *sql.Tx, repo string) (*Turn, error) {
	if _, err := tx.ExecContext(ctx, "INSERT INTO merge_turns(repo) VALUES($1) ON CONFLICT(repo) DO NOTHING", repo); err != nil {
		return nil, err
	}
	turn := &Turn{}
	var expires sql.NullTime
	var waiters []byte
	err := tx.QueryRowContext(ctx, "SELECT repo,holder_host,job_id,request_id,epoch,expires_at,waiters FROM merge_turns WHERE repo=$1 FOR UPDATE", repo).Scan(&turn.Repo, &turn.HolderHost, &turn.JobID, &turn.RequestID, &turn.Epoch, &expires, &waiters)
	if err != nil {
		return nil, err
	}
	turn.ExpiresAt = nullableISO(expires)
	if err = json.Unmarshal(waiters, &turn.waiters); err != nil {
		return nil, err
	}
	if turn.waiters == nil {
		turn.waiters = map[string]waiter{}
	}
	return turn, nil
}

func sameHolder(turn *Turn, who TurnRequest) bool {
	return turn.HolderHost != nil && *turn.HolderHost == who.Host && same(turn.JobID, who.JobID) && same(turn.RequestID, who.RequestID)
}
func held(turn *Turn, at time.Time) bool {
	if turn.HolderHost == nil || turn.ExpiresAt == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, *turn.ExpiresAt)
	return err == nil && expires.After(at)
}

func (s *Store) AcquireTurn(ctx context.Context, repo string, who TurnRequest) (*TurnResult, error) {
	at := s.options.Now()
	var result *TurnResult
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		actual, err := checkHolder(ctx, tx, who, at)
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual, repo) {
			return contract.NewProblem(400, "ジョブ・依頼と番のリポジトリが違います。", "")
		}
		turn, err := readTurn(ctx, tx, actual)
		if err != nil {
			return err
		}
		if who.JobID != nil {
			refused, err := s.refuseTurnForFinalApprovals(ctx, tx, actual, turn, who, at)
			if err != nil {
				return err
			}
			if refused != nil {
				result = refused
				return nil
			}
		}
		if held(turn, at) && sameHolder(turn, who) {
			result = &TurnResult{Granted: true, Turn: &Lease{Epoch: turn.Epoch, ExpiresAt: *turn.ExpiresAt}}
			return nil
		}
		for host, w := range turn.waiters {
			stamp, err := time.Parse(time.RFC3339Nano, w.At)
			if err != nil || !stamp.Add(s.options.WaitDuration).After(at) {
				delete(turn.waiters, host)
			}
		}
		var holder *Holder
		if held(turn, at) {
			holder = &Holder{Host: *turn.HolderHost, JobID: turn.JobID, RequestID: turn.RequestID, ExpiresAt: *turn.ExpiresAt}
		} else {
			holder, err = s.preferred(ctx, tx, actual, turn.waiters, who.Host, at)
			if err != nil {
				return err
			}
		}
		if holder != nil {
			previous, exists := turn.waiters[who.Host]
			since := iso(at)
			if exists {
				since = previous.Since
			}
			turn.waiters[who.Host] = waiter{JobID: who.JobID, RequestID: who.RequestID, Since: since, At: iso(at)}
			body, err := jsonValue(turn.waiters)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE merge_turns SET waiters=$2::jsonb WHERE repo=$1", actual, body); err != nil {
				return err
			}
			result = &TurnResult{Granted: false, Holder: holder}
			return nil
		}
		delete(turn.waiters, who.Host)
		body, err := jsonValue(turn.waiters)
		if err != nil {
			return err
		}
		var epoch int64
		var expires time.Time
		err = tx.QueryRowContext(ctx, `UPDATE merge_turns SET holder_host=$2,job_id=$3,request_id=$4,epoch=epoch+1,expires_at=$5,waiters=$6::jsonb WHERE repo=$1 RETURNING epoch,expires_at`, actual, who.Host, who.JobID, who.RequestID, at.Add(s.options.TurnDuration), body).Scan(&epoch, &expires)
		if err != nil {
			return err
		}
		result = &TurnResult{Granted: true, Turn: &Lease{Epoch: epoch, ExpiresAt: iso(expires)}}
		return nil
	})
	return result, err
}

// refuseTurnForFinalApprovals は最終承認が残る依頼の番の要求を断り、待ちと番を手放して依頼を最終承認待ちにする。残っていなければ nil を返す。
func (s *Store) refuseTurnForFinalApprovals(ctx context.Context, tx *sql.Tx, actual string, turn *Turn, who TurnRequest, at time.Time) (*TurnResult, error) {
	job, err := readJob(ctx, tx, *who.JobID, false)
	if err != nil {
		return nil, err
	}
	pending, err := PendingFinalApprovals(job)
	if err != nil {
		return nil, contract.NewProblem(409, "完了計画を確認できません。", "")
	}
	if len(pending) > 0 {
		if _, waiting := turn.waiters[who.Host]; waiting {
			delete(turn.waiters, who.Host)
			body, err := jsonValue(turn.waiters)
			if err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE merge_turns SET waiters=$2::jsonb WHERE repo=$1", actual, body); err != nil {
				return nil, err
			}
		}
		if sameHolder(turn, who) {
			if _, err = tx.ExecContext(ctx, "UPDATE merge_turns SET holder_host=NULL,job_id=NULL,request_id=NULL,expires_at=NULL WHERE repo=$1", actual); err != nil {
				return nil, err
			}
		}
		reasons := make([]string, 0, len(pending))
		for _, item := range pending {
			reasons = append(reasons, item.Reason)
		}
		// The approval can only be given from the screen, which shows it for held jobs.
		// Holding the job ends the worker's lease so it no longer occupies a PC slot.
		if err = s.holdForFinalApproval(ctx, tx, job, who.Host, pending, at); err != nil {
			return nil, err
		}
		return &TurnResult{Granted: false, Reason: strings.Join(reasons, "；")}, nil
	}
	return nil, nil
}

func (s *Store) RenewTurn(ctx context.Context, repo string, who TurnRequest) (*Lease, error) {
	return s.keepTurn(ctx, repo, who, false)
}
func (s *Store) ReleaseTurn(ctx context.Context, repo string, who TurnRequest) error {
	_, err := s.keepTurn(ctx, repo, who, true)
	return err
}

func (s *Store) keepTurn(ctx context.Context, repo string, who TurnRequest, release bool) (*Lease, error) {
	if who.TurnEpoch < 1 || who.TurnEpoch > maxSafeInteger {
		return nil, contract.NewProblem(400, "turnEpoch は1以上の整数にしてください。", "")
	}
	at := s.options.Now()
	var lease *Lease
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		actual, err := checkHolder(ctx, tx, who, at)
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual, repo) {
			return contract.NewProblem(400, "ジョブ・依頼と番のリポジトリが違います。", "")
		}
		turn, err := readTurn(ctx, tx, actual)
		if err != nil {
			return err
		}
		if release && turn.HolderHost == nil && turn.Epoch == who.TurnEpoch {
			return nil
		}
		if !held(turn, at) || !sameHolder(turn, who) || turn.Epoch != who.TurnEpoch {
			return contract.NewProblem(409, "マージの番はもうありません。", "turn_lost")
		}
		if release {
			_, err = tx.ExecContext(ctx, "UPDATE merge_turns SET holder_host=NULL,job_id=NULL,request_id=NULL,expires_at=NULL WHERE repo=$1", actual)
			return err
		}
		expires := at.Add(s.options.TurnDuration)
		if _, err = tx.ExecContext(ctx, "UPDATE merge_turns SET expires_at=$2 WHERE repo=$1", actual, expires); err != nil {
			return err
		}
		lease = &Lease{Epoch: turn.Epoch, ExpiresAt: iso(expires)}
		return nil
	})
	return lease, err
}

func (s *Store) preferred(ctx context.Context, tx *sql.Tx, repo string, waiters map[string]waiter, host string, at time.Time) (*Holder, error) {
	drained, err := s.hostDrained(ctx, tx, host, at)
	if err != nil || drained {
		return nil, err
	}
	hosts := make([]string, 0, len(waiters))
	for candidate := range waiters {
		hosts = append(hosts, candidate)
	}
	sort.SliceStable(hosts, func(i, j int) bool { return waiters[hosts[i]].Since < waiters[hosts[j]].Since })
	for _, candidate := range hosts {
		if candidate == host {
			continue
		}
		drained, err = s.hostDrained(ctx, tx, candidate, at)
		if err != nil {
			return nil, err
		}
		if !drained {
			continue
		}
		w := waiters[candidate]
		var exists bool
		if w.JobID != nil {
			err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1 AND repo=$2 AND status='running' AND lease_host=$3 AND lease_expires_at>$4)", *w.JobID, repo, candidate, at).Scan(&exists)
		} else if w.RequestID != nil {
			err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM requests WHERE id=$1 AND repo=$2 AND status='claimed' AND claimed_by=$3)", *w.RequestID, repo, candidate).Scan(&exists)
		} else {
			continue
		}
		if err != nil {
			return nil, err
		}
		if exists {
			stamp, err := time.Parse(time.RFC3339Nano, w.At)
			if err != nil {
				continue
			}
			return &Holder{Host: candidate, JobID: w.JobID, RequestID: w.RequestID, ExpiresAt: iso(stamp.Add(s.options.WaitDuration))}, nil
		}
	}
	return nil, nil
}

func (s *Store) hostDrained(ctx context.Context, tx *sql.Tx, host string, at time.Time) (bool, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT drain FROM hosts WHERE host=$1", host).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var mark map[string]any
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &mark); err != nil {
			return false, err
		}
	}
	return activeDrain(mark, at, s.options.DrainDuration), nil
}

func activeDrain(mark map[string]any, at time.Time, duration time.Duration) bool {
	return hosts.ActiveDrain(mark, at, duration) != nil
}
