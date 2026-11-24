package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
	"time"
)

type reservationAge struct {
	Mark  int64
	Since time.Time
	ID    int64
}
type reservation struct {
	ctx            context.Context
	tx             *sql.Tx
	host           string
	at             time.Time
	a              *fleet.Assignments
	ages           map[int64]reservationAge
	resumed        map[int64]bool
	resumeOwners   map[int64]string
	resumeAnywhere map[int64]bool
	published      map[int64]bool
	running        []*Job
	eligible       func(*Job) (bool, error)
	reservations   []*Job
	holders        map[string]*Job
	fallback       *Job
	fallbackReason string
	fallbackBy     int64
}

func older(a, b reservationAge) bool {
	if a.Mark != b.Mark {
		return a.Mark < b.Mark
	}
	if !a.Since.Equal(b.Since) {
		return a.Since.Before(b.Since)
	}
	return a.ID < b.ID
}
func (r *reservation) overlap(a, b *Job) bool {
	return strings.EqualFold(a.Repo, b.Repo) && ScopeOverlap(a.Scope, b.Scope, r.a.Exempt(a.Repo))
}
func estimatedWildcard(job *Job) bool {
	return job.ScopeSource == "estimated" && oneOf("*", job.Scope...)
}
func (r *reservation) waiting(job *Job) bool {
	return !r.resumed[job.ID] && r.at.Sub(r.ages[job.ID].Since) >= 30*time.Minute
}
func resetsReservationAge(kind string, data map[string]any) bool {
	return kind == "requeued" || kind == "state" && data["to"] == "queued" || kind == "control" && (data["action"] == "retry" || data["action"] == "resume" || data["action"] == "approve_deletions" || data["action"] == "permission_approval")
}
func newReservation(ctx context.Context, tx *sql.Tx, queued, running []*Job, a *fleet.Assignments, host string, at time.Time) (*reservation, error) {
	r := &reservation{ctx: ctx, tx: tx, host: host, at: at, a: a, ages: map[int64]reservationAge{}, resumed: map[int64]bool{}, resumeOwners: map[int64]string{}, resumeAnywhere: map[int64]bool{}, holders: map[string]*Job{}}
	var err error
	r.published, err = publishedImplementations(ctx, tx, append(append([]*Job{}, running...), queued...))
	if err != nil {
		return nil, err
	}
	r.running = running
	h, err := r.loadReservationHistory(ctx, tx, queued)
	if err != nil {
		return nil, err
	}
	if err = r.markPublicationResumes(ctx, tx, queued, h.events, host); err != nil {
		return nil, err
	}
	r.selectReservations(queued, h.reimplement, h.expired)
	return r, nil
}

// reservationEvent は予約の判断に使う依頼の出来事1件。
type reservationEvent struct {
	Seq        int64
	Kind, Host string
	Data       map[string]any
}

// reservationHistory は待ちの依頼ごとの出来事と、再実装の見込み・lease 切れで戻ったかの印。
type reservationHistory struct {
	events               map[int64][]reservationEvent
	reimplement, expired map[int64]bool
}

// loadReservationHistory は待ちの依頼の出来事を読み、出来事の一覧・再実装の見込み・lease 切れで戻った印を返し、待ち始めた時刻と範囲待ちから戻った印を r.ages に決める。lease 切れで戻った依頼は貸し出し前の待ちを引き継ぐ。
func (r *reservation) loadReservationHistory(ctx context.Context, tx *sql.Tx, queued []*Job) (reservationHistory, error) {
	var err error
	ids := []int64{}
	for _, job := range queued {
		stamp, _ := time.Parse(time.RFC3339Nano, job.FiledAt)
		r.ages[job.ID] = reservationAge{Mark: 1 << 62, Since: stamp, ID: job.ID}
		ids = append(ids, job.ID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT job_id,seq,kind,host,at,data FROM job_events WHERE job_id=ANY($1::bigint[])AND(kind IN('requeued','lease_expired','leased','state')OR kind='control'AND data->>'action'IN('retry','resume','approve_deletions','permission_approval','instruction'))ORDER BY seq`, ids)
	if err != nil {
		return reservationHistory{}, err
	}
	events := map[int64][]reservationEvent{}
	lastMark := map[int64]int64{}
	lastKind := map[int64]string{}
	type beforeLease struct {
		age  reservationAge
		kind string
		mark int64
	}
	before := map[int64]beforeLease{}
	reimplement := map[int64]bool{}
	epochs := map[int64]int64{}
	expired := map[int64]bool{}
	for _, job := range queued {
		reimplement[job.ID] = reimplementationExpected(job)
		epochs[job.ID] = job.LeaseEpoch
	}
	for rows.Next() {
		var id, seq int64
		var kind string
		var owner *string
		var stamp time.Time
		var raw []byte
		if err = rows.Scan(&id, &seq, &kind, &owner, &stamp, &raw); err != nil {
			rows.Close()
			return reservationHistory{}, err
		}
		var data map[string]any
		if err = json.Unmarshal(raw, &data); err != nil {
			rows.Close()
			return reservationHistory{}, err
		}
		by := ""
		if owner != nil {
			by = *owner
		}
		events[id] = append(events[id], reservationEvent{seq, kind, by, data})
		if kind == "leased" {
			expired[id] = false
			before[id] = beforeLease{r.ages[id], lastKind[id], lastMark[id]}
		}
		if kind == "requeued" || kind == "lease_expired" || kind == "leased" {
			lastMark[id] = seq
			lastKind[id] = kind
			if kind == "requeued" && data["from"] == "scope_wait" {
				age := r.ages[id]
				age.Mark = seq
				r.ages[id] = age
			}
		}
		if kind == "lease_expired" && data["to"] == "queued" {
			expired[id] = true
			if saved, ok := before[id]; ok {
				r.ages[id], lastKind[id], lastMark[id] = saved.age, saved.kind, saved.mark
				delete(before, id)
			}
		}
		if resetsReservationAge(kind, data) {
			expired[id] = false
		}
		age := r.ages[id]
		preserved := false
		if reimplement[id] && kind == "state" && data["to"] == "queued" && data["epoch"] == float64(epochs[id]) && lastKind[id] == "leased" {
			expired[id] = false
			if saved, ok := before[id]; ok {
				age, lastKind[id], lastMark[id] = saved.age, saved.kind, saved.mark
				r.ages[id] = age
				preserved = true
			}
		}
		if !preserved && resetsReservationAge(kind, data) && stamp.After(age.Since) {
			age.Since = stamp
			r.ages[id] = age
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return reservationHistory{}, err
	}
	for id, age := range r.ages {
		if lastKind[id] != "requeued" || lastMark[id] != age.Mark {
			age.Mark = 1 << 62
			r.ages[id] = age
		}
	}
	return reservationHistory{events: events, reimplement: reimplement, expired: expired}, nil
}

// markPublicationResumes は公開待ちの依頼が再実装せずに続けられるか、どのPCで続けられるかを決めて記録する。
func (r *reservation) markPublicationResumes(ctx context.Context, tx *sql.Tx, queued []*Job, events map[int64][]reservationEvent, host string) error {
	var err error
	for _, job := range queued {
		if job.View["releasePending"] != true && job.View["publishPending"] != true {
			continue
		}
		history := events[job.ID]
		var lastLease int64
		owner := ""
		for _, event := range history {
			if event.Kind == "leased" {
				lastLease = event.Seq
				owner = event.Host
			}
		}
		retries := []historyEvent{}
		instructions := false
		afterCount := 0
		for _, event := range history {
			if event.Seq <= lastLease || event.Kind != "control" || (event.Data["action"] != "retry" && event.Data["action"] != "instruction") {
				continue
			}
			afterCount++
			if event.Data["action"] == "instruction" {
				instructions = true
			} else {
				retries = append(retries, historyEvent{Data: event.Data})
			}
		}
		reimplements := instructions
		if len(retries) == 0 {
			reimplements = afterCount > 0
		} else {
			for _, retry := range retries {
				if retry.Data["instruction"] == true || retry.Data["fromBase"] == true || retry.Data["retest"] == true {
					reimplements = true
				}
			}
		}
		// A retry keeps publication flags and the old tuple. Current-generation
		// retry/retest markers remain protected until a fresh publication receipt.
		if r.published[job.ID] {
			reimplements = false
		} else if reimplementationExpected(job) {
			reimplements = true
		}
		artifact, known := artifactOf(job)
		publication := publicationTuple(job) && known && artifact.Generation == job.Generation && artifactPublished(job, artifact)
		if !reimplements {
			r.resumeOwners[job.ID] = owner
			r.resumeAnywhere[job.ID] = publication
		}
		r.resumed[job.ID] = (owner == host || publication) && !reimplements
		job.View["publicationResume"] = r.resumed[job.ID]
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{publicationResume}',$2::jsonb)WHERE id=$1", job.ID, transferJSON(r.resumed[job.ID])); err != nil {
			return err
		}
	}
	return nil
}

// selectReservations は古い順に、範囲を予約する依頼と、塞がれていてリポジトリを待たせる依頼を選ぶ。
func (r *reservation) selectReservations(queued []*Job, reimplement, expired map[int64]bool) {
	ordered := append([]*Job{}, queued...)
	sort.SliceStable(ordered, func(i, j int) bool { return older(r.ages[ordered[i].ID], r.ages[ordered[j].ID]) })
	for _, job := range ordered {
		// 見積もり失敗で付く * は実際の範囲ではなく、1件が同じリポジトリを止め続けるため予約の元にしない。
		if estimatedWildcard(job) || r.at.Sub(r.ages[job.ID].Since) > 2*time.Hour {
			continue
		}
		immediate := reimplement[job.ID] && !expired[job.ID] && !r.published[job.ID] && !r.resumed[job.ID]
		if (r.ages[job.ID].Mark != 1<<62 || r.waiting(job) || immediate) && !r.blocked(job) {
			r.reservations = append(r.reservations, job)
		}
		if (r.waiting(job) || immediate) && r.blocked(job) {
			key := strings.ToLower(job.Repo)
			if r.holders[key] == nil {
				r.holders[key] = job
			}
		}
	}
}

type historyEvent struct{ Data map[string]any }

// Record candidates that would have been deferred by the selected owner even
// when the dispatch order reaches the owner before those candidates.
func (r *reservation) recordOvertaken(owner *Job, queued []*Job) error {
	if !reimplementationExpected(owner) {
		return nil
	}
	reserved := false
	for _, job := range r.reservations {
		if job.ID == owner.ID {
			reserved = true
			break
		}
	}
	if !reserved {
		return nil
	}
	for _, job := range queued {
		if job.ID == owner.ID || r.resumed[job.ID] || job.Priority > owner.Priority || !older(r.ages[owner.ID], r.ages[job.ID]) || !r.overlap(owner, job) {
			continue
		}
		ready, err := r.canReserve(job)
		if err != nil {
			return err
		}
		if ready {
			if err := r.record(job, owner.ID, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *reservation) record(job *Job, by int64, release bool) error {
	data := map[string]any{"reason": "scope_reserved", "by": by}
	if release {
		data["release"] = true
	}
	return recordDispatchSkip(r.ctx, r.tx, job, r.host, data, r.at)
}
func (r *reservation) deferJob(job *Job, reason string) (bool, error) {
	if r.resumed[job.ID] {
		return false, nil
	}
	holder := r.holders[strings.ToLower(job.Repo)]
	if holder != nil && holder.ID != job.ID && job.Priority <= holder.Priority && r.overlap(holder, job) && (!r.waiting(job) || older(r.ages[holder.ID], r.ages[job.ID])) {
		ready, err := r.canReserve(holder)
		if err != nil {
			return false, err
		}
		if ready {
			return true, r.record(job, holder.ID, false)
		}
	}
	for _, by := range r.reservations {
		if by.ID != job.ID && older(r.ages[by.ID], r.ages[job.ID]) && job.Priority <= by.Priority && r.overlap(by, job) {
			ready, err := r.canReserve(by)
			if err != nil {
				return false, err
			}
			if !ready {
				continue
			}
			if err := r.record(job, by.ID, false); err != nil {
				return false, err
			}
			if r.fallback == nil {
				r.fallback, r.fallbackReason, r.fallbackBy = job, reason, by.ID
			}
			return true, nil
		}
	}
	return false, nil
}

// The same published-running exclusion is used when building reservations and
// when dispatching a candidate. Published jobs still consume capacity.
func (r *reservation) busyOverlap(job, busy *Job) bool {
	return !r.resumed[job.ID] && !r.published[busy.ID] && r.overlap(job, busy)
}
func (r *reservation) blocked(job *Job) bool {
	for _, busy := range r.running {
		if r.busyOverlap(job, busy) {
			return true
		}
	}
	return false
}
func (r *reservation) canReserve(job *Job) (bool, error) {
	if r.blocked(job) {
		return false, nil
	}
	if r.eligible != nil {
		return r.eligible(job)
	}
	// Standalone reservation callers use the same gates as Next. Next supplies
	// its already-filtered cycle snapshot and configured drain duration above.
	var dependency bool
	if err := r.tx.QueryRowContext(r.ctx, `SELECT EXISTS(SELECT 1 FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id=$1 AND NOT (`+DependencyPairReadySQL+`))`, job.ID).Scan(&dependency); err != nil || dependency {
		return false, err
	}
	states, err := dispatchHosts(r.ctx, r.tx)
	if err != nil {
		return false, err
	}
	entry := r.a.Hosts[r.host]
	if entry == nil {
		return false, nil
	}
	e := dispatchEligibility{ctx: r.ctx, tx: r.tx, host: r.host, capacity: entry.Max, states: states, running: r.running, a: r.a, at: r.at, drain: 150 * time.Minute, reservation: r}
	decision, err := e.check(job, false)
	return decision.ready, err
}
