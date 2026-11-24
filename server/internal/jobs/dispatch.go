package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

// heartbeatFreshness treats a host as reachable for two Worker heartbeats (one
// every 3 minutes). A shorter window rejects two of every three polls from a
// healthy Worker and moves its work to another host as stale.
const heartbeatFreshness = 6 * time.Minute

type hostState struct {
	Host                      string
	Ready                     bool
	Drain, Capabilities, Hold map[string]any
	Heartbeat                 time.Time
	Capacity                  int
}

func dispatchHosts(ctx context.Context, tx *sql.Tx) (map[string]hostState, error) {
	rows, err := tx.QueryContext(ctx, "SELECT host,ready,drain,capabilities,intake_hold,heartbeat_at,capacity FROM hosts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]hostState{}
	for rows.Next() {
		var state hostState
		var drain, capabilities, hold []byte
		var capacity sql.NullInt64
		if err = rows.Scan(&state.Host, &state.Ready, &drain, &capabilities, &hold, &state.Heartbeat, &capacity); err != nil {
			return nil, err
		}
		state.Capacity = -1
		if capacity.Valid {
			state.Capacity = int(capacity.Int64)
		}
		for _, item := range []struct {
			raw    []byte
			target *map[string]any
		}{{drain, &state.Drain}, {capabilities, &state.Capabilities}, {hold, &state.Hold}} {
			raw, target := string(item.raw), item.target
			if raw != "" {
				if err = json.Unmarshal([]byte(raw), target); err != nil {
					return nil, err
				}
			}
		}
		result[state.Host] = state
	}
	return result, rows.Err()
}

func dispatchJobs(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]*Job, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []*Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, job)
	}
	return list, rows.Err()
}

func availableSlots(host, repo string, state map[string]hostState, running []*Job, a *fleet.Assignments, at time.Time) int {
	row, exists := state[host]
	entry := a.Hosts[host]
	if !exists || entry == nil || !row.Ready || activeDrain(row.Drain, at, 150*time.Minute) || len(fleet.List(row.Hold["reasons"])) > 0 || at.Sub(row.Heartbeat) >= heartbeatFreshness {
		return 0
	}
	if list, reported := row.Capabilities["repositories"].([]any); reported {
		found := false
		for _, value := range list {
			if strings.EqualFold(fleet.Text(value), repo) {
				found = true
			}
		}
		if !found {
			return 0
		}
	}
	count, repoCount := 0, 0
	for _, job := range running {
		if job.LeaseHost != nil && *job.LeaseHost == host {
			count++
			if strings.EqualFold(job.Repo, repo) {
				repoCount++
			}
		}
	}
	limit := entry.Max
	if row.Capacity >= 0 && row.Capacity < limit {
		limit = row.Capacity
	}
	return min(limit-count, a.RepoLimit(host, repo)-repoCount)
}

func availableHost(host, repo string, state map[string]hostState, running []*Job, a *fleet.Assignments, at time.Time) bool {
	return availableSlots(host, repo, state, running, a, at) > 0
}

func (s *Store) Next(ctx context.Context, host string, capacity int, a *fleet.Assignments, restrict ...[]string) (*Job, *Lease, error) {
	return s.next(ctx, host, capacity, a, nil, restrict...)
}

// Delivery contains the details that must be readable before a job is leased.
type Delivery struct {
	WeightedUsage    any
	DependencyMerges []any
}

// NextForDelivery rolls back the acquisition if the delivery details cannot be read.
func (s *Store) NextForDelivery(ctx context.Context, host string, capacity int, a *fleet.Assignments, weights map[string]any, restrict ...[]string) (*Job, *Lease, Delivery, error) {
	var details Delivery
	job, lease, err := s.next(ctx, host, capacity, a, func(ctx context.Context, tx *sql.Tx, job *Job) error {
		// Avoid holding the dispatch locks through an unbounded delivery read.
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var err error
		details.WeightedUsage, err = usageSnapshot(ctx, tx, job, weights)
		if err == nil {
			details.DependencyMerges, err = dependencyMerges(ctx, tx, job.ID)
		}
		if err != nil {
			return err
		}
		if job.LeaseExpiresAt == nil {
			return errors.New("配布する仕事の貸出期限がありません")
		}
		expires, err := time.Parse(time.RFC3339Nano, *job.LeaseExpiresAt)
		if err != nil {
			return err
		}
		if !expires.After(s.options.Now()) {
			return contract.NewProblem(503, "配布を準備する間に貸出期限が切れました。", "")
		}
		return ctx.Err()
	}, restrict...)
	return job, lease, details, err
}

func (s *Store) next(ctx context.Context, host string, capacity int, a *fleet.Assignments, prepareDelivery func(context.Context, *sql.Tx, *Job) error, restrict ...[]string) (*Job, *Lease, error) {
	if capacity < 1 || capacity > 9007199254740991 {
		return nil, nil, contract.NewProblem(400, "capacityは1以上の整数です。", "")
	}
	prepared, prepareErr := s.prepareTransfers(ctx)
	if prepareErr != nil {
		return nil, nil, prepareErr
	}
	at := s.options.Now().UTC().Truncate(time.Millisecond)
	var picked *Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := refreshTransfers(ctx, tx, prepared, iso(at)); err != nil {
			return err
		}
		states, err := dispatchHosts(ctx, tx)
		if err != nil {
			return err
		}
		own, exists := states[host]
		if !exists || !own.Ready || activeDrain(own.Drain, at, s.options.DrainDuration) {
			return nil
		}
		entry := a.Hosts[host]
		if entry == nil {
			return nil
		}
		repos, err := dispatchRepositories(ctx, tx, host, own, a, at, restrict...)
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			return nil
		}
		if err = lockDispatch(ctx, tx, host, repos); err != nil {
			return err
		}
		overloaded, err := dispatchOverloaded(ctx, tx, host, at, fleet.Object(fleet.Object(a.Output["dispatch"])["loadLimit"]), time.Duration(fleet.Number(fleet.Object(a.Output["dispatch"])["leaseMinutes"]))*time.Minute)
		if err != nil {
			return err
		}
		if overloaded {
			return nil
		}
		lowerRepos := make([]string, len(repos))
		for i, repo := range repos {
			lowerRepos[i] = strings.ToLower(repo)
		}
		all, err := dispatchJobs(ctx, tx, `WHERE status='queued' AND kind<>'investigation' AND coalesce(view->>'workType','')<>'investigation'
AND coalesce((view->'acceptanceTransferWait'->>'blocking')::boolean,false)=false
AND lower(repo)=ANY($1::text[]) ORDER BY priority DESC,filed_at,id`, lowerRepos)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return nil
		}
		// refreshTransfers holds the cycle table lock through leasing. Read
		// publication and instruction state only after that lock is acquired.
		running, err := dispatchJobs(ctx, tx, "WHERE status='running'")
		if err != nil {
			return err
		}
		count := 0
		for _, job := range running {
			if job.LeaseHost != nil && *job.LeaseHost == host {
				count++
			}
		}
		limit := entry.Max
		if capacity < limit {
			limit = capacity
		}
		if count < limit {
			ctx, err = loadDispatchSkipCache(ctx, tx, all, at)
			if err != nil {
				return err
			}
		}
		blocked, explicit, err := loadDependencySnapshot(ctx, tx, all)
		if err != nil {
			return err
		}
		full := count >= limit
		queued, reservable, fullSkips, err := splitDispatchQueue(ctx, tx, all, blocked, explicit, full, host, at)
		if err != nil {
			return err
		}
		if full {
			if len(fleet.List(own.Hold["reasons"])) == 0 && at.Sub(own.Heartbeat) < heartbeatFreshness {
				for _, job := range queued {
					if artifactDispatchable(job, host) {
						fullSkips = append(fullSkips, skipBatchEntry{job.ID, slotData(host, job.Repo, capacity, states, running, a, true)})
					}
				}
			}
			return recordFullHostSkips(ctx, tx, fullSkips, host, at)
		}
		order, err := loadDispatchOrder(ctx, tx, queued)
		if err != nil {
			return err
		}
		rank := func(repo string) int {
			for i, candidate := range a.Implementers[repo] {
				if candidate == host {
					return i
				}
			}
			return 1 << 30
		}
		dispatchAt := s.options.Now().UTC()
		sort.SliceStable(queued, func(i, j int) bool {
			return dispatchBefore(queued[i], queued[j], order, rank, dispatchAt)
		})
		reservation, err := newReservation(ctx, tx, reservable, running, a, host, at)
		if err != nil {
			return err
		}
		eligibility := dispatchEligibility{ctx: ctx, tx: tx, host: host, capacity: capacity, states: states, running: running, a: a, at: at, drain: s.options.DrainDuration, reservation: reservation, explicit: explicit}
		reservation.eligible = func(job *Job) (bool, error) {
			decision, err := eligibility.check(job, false)
			return decision.ready, err
		}
		picked, err = pickDispatchCandidate(ctx, tx, queued, reservable, reservation, eligibility, host, a, at)
		if err == nil && picked != nil && prepareDelivery != nil {
			return prepareDelivery(ctx, tx, picked)
		}
		return err
	})
	if err != nil || picked == nil {
		return nil, nil, err
	}
	return picked, &Lease{Epoch: picked.LeaseEpoch, ExpiresAt: *picked.LeaseExpiresAt}, nil
}

// lockDispatch はリポジトリとPCの配布の鍵を、デッドロックしない決まった順に取る。
func lockDispatch(ctx context.Context, tx *sql.Tx, host string, repos []string) error {
	locks := append([]string{}, repos...)
	locks = append(locks, "host:"+host)
	for i := range locks {
		locks[i] = strings.ToLower(locks[i])
	}
	sort.Strings(locks)
	for _, name := range locks {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", jobsAdvisoryLockKey(name)); err != nil {
			return err
		}
	}
	return nil
}

// splitDispatchQueue は待ちの依頼から依存で止まるものを見送りとして記録し（PCが満杯なら後でまとめて記録する分を返し）、配布の候補と予約できる候補を返す。
func splitDispatchQueue(ctx context.Context, tx *sql.Tx, all []*Job, blocked, explicit map[int64]bool, full bool, host string, at time.Time) ([]*Job, []*Job, []skipBatchEntry, error) {
	queued := make([]*Job, 0, len(all))
	reservable := make([]*Job, 0, len(all))
	fullSkips := []skipBatchEntry{}
	for _, job := range all {
		if blocked[job.ID] || (!explicit[job.ID] && full) {
			if full {
				fullSkips = append(fullSkips, skipBatchEntry{job.ID, map[string]any{"reason": "dependency"}})
			} else if err := recordDispatchSkip(ctx, tx, job, host, map[string]any{"reason": "dependency"}, at); err != nil {
				return nil, nil, nil, err
			}
			continue
		}
		queued = append(queued, job)
		if explicit[job.ID] {
			reservable = append(reservable, job)
		}
	}
	return queued, reservable, fullSkips, nil
}

// pickDispatchCandidate は並べた候補（最後に予約の代替）を順に検査し、最初に貸し出せた依頼を返す。
func pickDispatchCandidate(ctx context.Context, tx *sql.Tx, queued, reservable []*Job, reservation *reservation, eligibility dispatchEligibility, host string, a *fleet.Assignments, at time.Time) (*Job, error) {
	candidates := append(append([]*Job{}, queued...), nil)
	for _, candidate := range candidates {
		fallback := candidate == nil
		if fallback {
			candidate = reservation.fallback
			if candidate == nil {
				break
			}
		}
		decision, err := eligibility.check(candidate, true)
		if err != nil {
			return nil, err
		}
		if !decision.ready {
			if decision.skip != nil {
				if err = recordDispatchSkip(ctx, tx, candidate, host, decision.skip, at); err != nil {
					return nil, err
				}
			}
			continue
		}
		reason := decision.reason
		if !fallback {
			deferred, err := reservation.deferJob(candidate, reason)
			if err != nil {
				return nil, err
			}
			if deferred {
				continue
			}
		}
		prevented, err := preventWildcardOvertaking(reservation, eligibility, reservable, candidate)
		if err != nil {
			return nil, err
		}
		if prevented {
			continue
		}
		if !fallback {
			if err = reservation.recordOvertaken(candidate, reservable); err != nil {
				return nil, err
			}
		}
		if fallback {
			if err = reservation.record(candidate, reservation.fallbackBy, true); err != nil {
				return nil, err
			}
		}
		view, err := prepareLeaseView(ctx, tx, candidate, host, reason, at)
		if err != nil {
			return nil, err
		}
		leased, err := leaseJob(ctx, tx, candidate, host, view, a, at)
		if err != nil {
			return nil, err
		}
		if leased == nil {
			continue
		}
		return leased, nil
	}
	return nil, nil
}

// dispatchRepositories はこのPCへ配ってよいリポジトリを、割り当て・呼び出しの絞り込み・PCの申告（申告が無ければ直近の引き取り失敗）で絞る。
func dispatchRepositories(ctx context.Context, tx *sql.Tx, host string, own hostState, a *fleet.Assignments, at time.Time, restrict ...[]string) ([]string, error) {
	repos := a.Repositories(host)
	if len(restrict) > 0 {
		allowed := map[string]bool{}
		for _, repo := range restrict[0] {
			allowed[strings.ToLower(repo)] = true
		}
		filtered := []string{}
		for _, repo := range repos {
			if allowed[strings.ToLower(repo)] {
				filtered = append(filtered, repo)
			}
		}
		repos = filtered
	}
	if list, reported := own.Capabilities["repositories"].([]any); reported {
		allowed := map[string]bool{}
		for _, value := range list {
			allowed[strings.ToLower(fleet.Text(value))] = true
		}
		filtered := []string{}
		for _, repo := range repos {
			if allowed[strings.ToLower(repo)] {
				filtered = append(filtered, repo)
			}
		}
		repos = filtered
	} else {
		rows, err := tx.QueryContext(ctx, "SELECT DISTINCT lower(j.repo) FROM job_events e JOIN jobs j ON j.id=e.job_id WHERE e.host=$1 AND e.kind='handed_back' AND e.data->>'reason'='adopt_failed' AND e.at>=$2", host, at.Add(-30*time.Minute))
		if err != nil {
			return nil, err
		}
		blocked := map[string]bool{}
		for rows.Next() {
			var repo string
			if err = rows.Scan(&repo); err != nil {
				rows.Close()
				return nil, err
			}
			blocked[repo] = true
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
		filtered := []string{}
		for _, repo := range repos {
			if !blocked[strings.ToLower(repo)] {
				filtered = append(filtered, repo)
			}
		}
		repos = filtered
	}
	return repos, nil
}

// prepareLeaseView は貸し出す依頼の状態を、PCの移動・成果物の照合の結果と最終承認待ちの解除で書き換え、その記録を残す。
func prepareLeaseView(ctx context.Context, tx *sql.Tx, candidate *Job, host, reason string, at time.Time) (map[string]any, error) {
	view := candidate.View
	if reset := fleet.Object(view["retryFromBase"]); reset["by"] == "dispatch" {
		delete(view, "retryFromBase")
	}
	mark := fleet.Object(view["continuation"])
	if mark["pending"] == true && fleet.Number(mark["generation"]) == float64(candidate.Generation) {
		mark["pending"], mark["to"] = false, host
		if previous := fleet.Text(mark["host"]); previous != "" && previous != host {
			kept := candidate.Branch != nil && *candidate.Branch != ""
			if !kept {
				view["retryFromBase"] = map[string]any{"generation": candidate.Generation, "by": "dispatch", "at": iso(at), "reason": reason, "from": previous}
			}
			if err := addEvent(ctx, tx, candidate.ID, host, "worktree_moved", map[string]any{"from": previous, "to": host, "reason": reason, "branchKept": kept}, at); err != nil {
				return nil, err
			}
		}
	}
	// After the move hint: a "dispatch" reset is not accepted by Report.
	unreportable, err := artifactUnreportable(ctx, tx, candidate, host)
	if err != nil {
		return nil, err
	}
	if unreportable {
		view["retryFromBase"] = map[string]any{"generation": candidate.Generation, "by": artifactResetBy, "at": iso(at), "reason": "artifact_unknown", "host": host}
		if err = addEvent(ctx, tx, candidate.ID, host, "artifact_reset", map[string]any{"generation": candidate.Generation, "reason": "artifact_unknown"}, at); err != nil {
			return nil, err
		}
	}
	// A new lease ends any earlier wait for a final approval.
	delete(view, FinalApprovalHoldKey)
	delete(view, FinalApprovalResumeKey)
	return view, nil
}

// leaseJob は待ちの依頼をこのPCへ貸し出して記録する。ほかの経路が先に状態を変えていたら nil を返す。
func leaseJob(ctx context.Context, tx *sql.Tx, candidate *Job, host string, view map[string]any, a *fleet.Assignments, at time.Time) (*Job, error) {
	body, err := jsonValue(view)
	if err != nil {
		return nil, err
	}
	leaseDuration := time.Duration(fleet.Number(fleet.Object(a.Output["dispatch"])["leaseMinutes"])) * time.Minute
	result, err := tx.ExecContext(ctx, "UPDATE jobs SET status='running',lease_host=$2,lease_epoch=lease_epoch+1,lease_expires_at=$3,progress_at=$4,updated_at=$4,view=($5::jsonb - 'dependencyMerges') || (CASE WHEN view ? 'dependencyMerges' THEN jsonb_build_object('dependencyMerges',view->'dependencyMerges') ELSE '{}'::jsonb END) WHERE id=$1 AND status='queued'", candidate.ID, host, at.Add(leaseDuration), at, body)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, nil
	}
	picked, err := readJob(ctx, tx, candidate.ID, false)
	if err != nil {
		return nil, err
	}
	diagnostic := map[string]any{"epoch": picked.LeaseEpoch, "generation": picked.Generation, "expiresAt": *picked.LeaseExpiresAt}
	if phase, ok := picked.View["phase"]; ok {
		diagnostic["phase"] = phase
	}
	if err = addEvent(ctx, tx, picked.ID, host, "leased", diagnostic, at); err != nil {
		return nil, err
	}
	return picked, nil
}

func continuationDecision(job *Job, host string, states map[string]hostState, a *fleet.Assignments, at time.Time, duration time.Duration, compatible func(hostState) string) (bool, bool, string) {
	mark := fleet.Object(job.View["continuation"])
	stamp, err := time.Parse(time.RFC3339Nano, fleet.Text(mark["at"]))
	if mark["pending"] != true || fleet.Number(mark["generation"]) != float64(job.Generation) || err != nil {
		return false, true, ""
	}
	owner := fleet.Text(mark["host"])
	if owner == host {
		return true, true, ""
	}
	assigned := false
	for _, candidate := range a.Implementers[job.Repo] {
		if candidate == owner {
			assigned = true
		}
	}
	if !assigned {
		return false, true, "not_implementer"
	}
	state, exists := states[owner]
	if !exists || !state.Ready {
		return false, true, "not_ready"
	}
	if reason := compatible(state); reason != "" {
		return false, true, reason
	}
	if activeDrain(state.Drain, at, duration) {
		return false, true, "drain"
	}
	if at.Sub(state.Heartbeat) >= heartbeatFreshness {
		return false, true, "stale"
	}
	if list, reported := state.Capabilities["repositories"].([]any); reported {
		found := false
		for _, value := range list {
			if strings.EqualFold(fleet.Text(value), job.Repo) {
				found = true
			}
		}
		if !found {
			return false, true, "not_assigned"
		}
	}
	if at.Sub(stamp) >= 20*time.Minute {
		return false, true, "grace"
	}
	return false, false, ""
}

// dispatchBefore keeps inherited priority and successor ordering ahead of age.
// An unawaited followup becomes eligible ahead of normal work one hour after filing.
func dispatchBefore(a, b *Job, order map[int64]dispatchOrder, rank func(string) int, at time.Time) bool {
	oa, ob := order[a.ID], order[b.ID]
	if oa.priority != ob.priority {
		return oa.priority > ob.priority
	}
	if oa.successors != ob.successors {
		return oa.successors > ob.successors
	}
	class := func(job *Job, o dispatchOrder) int {
		if !deferredFollowup(job, o) {
			return 1
		}
		filed := job.filedAt
		if filed.IsZero() {
			filed, _ = time.Parse(time.RFC3339Nano, job.FiledAt)
		}
		if !filed.IsZero() && !at.Before(filed.Add(time.Hour)) {
			return 0
		}
		return 2
	}
	ca, cb := class(a, oa), class(b, ob)
	if ca != cb {
		return ca < cb
	}
	if ca == 0 {
		return oa.filedRank < ob.filedRank
	}
	if rank(a.Repo) != rank(b.Repo) {
		return rank(a.Repo) < rank(b.Repo)
	}
	return oa.filedRank < ob.filedRank
}

func deferredFollowup(job *Job, order dispatchOrder) bool {
	return job.Kind == "followup" && order.successors == 0
}
