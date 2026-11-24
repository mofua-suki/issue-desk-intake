package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

type event struct {
	Seq, Job   int64
	Host, Kind string
	At         time.Time
	Data       map[string]any
}
type job struct {
	ID, Issue  int64
	Repo, Tier string
	Filed      time.Time
	Result     map[string]any
}
type deployment struct {
	Repo, Commit, Status string
	Finished             sql.NullTime
}
type records struct {
	Deployments    []deployment
	Jobs           map[int64]job
	Events         []event
	Oldest, Latest time.Time
	End            time.Time
}

func (s *Service) read(ctx context.Context, project string, to time.Time) (records, error) {
	out := records{Jobs: map[int64]job{}, End: time.Now()}
	if s.Now != nil {
		out.End = s.Now()
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = readWorkJobs(ctx, tx, project, to, &out); err != nil {
		return out, err
	}
	if err = readWorkEvents(ctx, tx, project, to, &out); err != nil {
		return out, err
	}
	if err = readAppDeployments(ctx, tx, project, &out); err != nil {
		return out, err
	}
	if err = readWorkerDeployments(ctx, tx, project, &out); err != nil {
		return out, err
	}
	if project == "" || project == "mofua-suki/issue-desk-intake" {
		if err = readIntakeDeployments(ctx, tx, &out); err != nil {
			return out, err
		}
	}
	var oldest sql.NullTime
	if err = tx.QueryRowContext(ctx, "SELECT min(at) FROM job_events").Scan(&oldest); err != nil {
		return out, err
	}
	if oldest.Valid {
		out.Oldest = oldest.Time
	}
	return out, tx.Commit()
}

// readWorkJobs reads the jobs filed before to.
func readWorkJobs(ctx context.Context, tx *sql.Tx, project string, to time.Time, out *records) error {
	rows, err := tx.QueryContext(ctx, "SELECT id,repo,issue,coalesce(tier,'unknown'),filed_at,result FROM jobs WHERE ($1='' OR repo=$1)AND filed_at<$2", project, to)
	if err != nil {
		return err
	}
	for rows.Next() {
		j := job{}
		var raw []byte
		if err = rows.Scan(&j.ID, &j.Repo, &j.Issue, &j.Tier, &j.Filed, &raw); err != nil {
			rows.Close()
			return err
		}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &j.Result); err != nil {
				rows.Close()
				return err
			}
		}
		out.Jobs[j.ID] = j
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

// readWorkEvents reads the job events before to and their time range.
func readWorkEvents(ctx context.Context, tx *sql.Tx, project string, to time.Time, out *records) error {
	rows, err := tx.QueryContext(ctx, "SELECT e.seq,e.job_id,coalesce(e.host,''),e.kind,e.at,e.data FROM job_events e JOIN jobs j ON j.id=e.job_id WHERE($1='' OR j.repo=$1)AND e.at<$2 ORDER BY e.at,e.seq", project, to)
	if err != nil {
		return err
	}
	for rows.Next() {
		e := event{}
		var raw []byte
		if err = rows.Scan(&e.Seq, &e.Job, &e.Host, &e.Kind, &e.At, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &e.Data); err != nil {
			rows.Close()
			return err
		}
		out.Events = append(out.Events, e)
		if out.Oldest.IsZero() || e.At.Before(out.Oldest) {
			out.Oldest = e.At
		}
		if e.At.After(out.Latest) {
			out.Latest = e.At
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

// readAppDeployments reads the recorded application deployments.
func readAppDeployments(ctx context.Context, tx *sql.Tx, project string, out *records) error {
	rows, err := tx.QueryContext(ctx, "SELECT repo,commit_sha,status,finished_at FROM app_deployments WHERE ($1='' OR repo=$1)", project)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d deployment
		if err = rows.Scan(&d.Repo, &d.Commit, &d.Status, &d.Finished); err != nil {
			rows.Close()
			return err
		}
		out.Deployments = append(out.Deployments, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

// readWorkerDeployments reads when the worker image reached the PCs.
func readWorkerDeployments(ctx context.Context, tx *sql.Tx, project string, out *records) error {
	// The worker and the intake update themselves outside app_deployments: the
	// worker image reaches each PC (host_image_history), and the intake records
	// every takeover of the leader lease (server_takeovers, see intakeDeployments).
	rows, err := tx.QueryContext(ctx, `SELECT 'mofua-suki/issue-desk-worker',coalesce(j.commit,left(h.tag,12)),min(h.received_at) FROM host_image_history h LEFT JOIN(SELECT DISTINCT result->>'mergeCommit' AS commit FROM jobs WHERE repo='mofua-suki/issue-desk-worker' AND length(result->>'mergeCommit')=40)j ON left(j.commit,12)=left(h.tag,12) WHERE h.status='success' AND($1='' OR $1='mofua-suki/issue-desk-worker') GROUP BY 2`, project)
	if err != nil {
		return err
	}
	for rows.Next() {
		d := deployment{Status: "deployed"}
		if err = rows.Scan(&d.Repo, &d.Commit, &d.Finished); err != nil {
			rows.Close()
			return err
		}
		out.Deployments = append(out.Deployments, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

// readIntakeDeployments reads the intake updates from the leader lease takeovers.
func readIntakeDeployments(ctx context.Context, tx *sql.Tx, out *records) error {
	rows, err := tx.QueryContext(ctx, "SELECT revision,at FROM server_takeovers ORDER BY at,id")
	if err != nil {
		return err
	}
	var takeovers []serverTakeover
	for rows.Next() {
		var t serverTakeover
		if err = rows.Scan(&t.Revision, &t.At); err != nil {
			rows.Close()
			return err
		}
		takeovers = append(takeovers, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	out.Deployments = append(out.Deployments, intakeDeployments(takeovers)...)
	return nil
}

type span struct {
	From, To time.Time
	Job      int64
	Ended    bool
}

func spans(r records, to time.Time) []span {
	open := map[int64]time.Time{}
	out := []span{}
	for _, e := range r.Events {
		if e.Kind == "leased" {
			if start, exists := open[e.Job]; exists {
				out = append(out, span{start, e.At, e.Job, false})
			}
			open[e.Job] = e.At
		}
		if e.Kind == "lease_expired" || e.Kind == "state" && e.Data["to"] != "running" {
			if start, exists := open[e.Job]; exists {
				out = append(out, span{start, e.At, e.Job, true})
				delete(open, e.Job)
			}
		}
	}
	for id, start := range open {
		out = append(out, span{start, to, id, false})
	}
	return out
}
func workPeriod(r records, p Period) map[string]any {
	out := p.Dates()
	if r.Oldest.IsZero() || !r.Oldest.Before(p.To) || !p.From.Before(r.End) {
		out["measured"] = nil
		return out
	}
	if p.From.Before(r.Oldest) {
		p.From = r.Oldest
	}
	if p.To.After(r.End) {
		p.To = r.End
	}
	c := workConcurrency(r, p)
	completed, waitCount, lead, wait := workLeadTimes(r, p)
	var ratio, leadMs, waitMs any
	if c.active > 0 {
		ratio = float64(c.parallel) / float64(c.active)
	}
	if completed > 0 {
		leadMs = float64(lead) / float64(completed)
	}
	if waitCount > 0 {
		waitMs = float64(wait) / float64(waitCount)
	}
	out["measured"] = map[string]any{"start": ISO(p.From), "end": ISO(p.To)}
	out["measuredMs"] = p.To.Sub(p.From).Milliseconds() - c.gapMs
	out["totalMs"] = c.totalMs
	out["activeMs"], out["parallelMs"], out["idleMs"] = c.active, c.parallel, c.idle
	out["gapMs"], out["stoppedMs"], out["gaps"] = c.gapMs, 0, c.unended
	out["ratio"], out["maxConcurrent"], out["runs"], out["unended"] = ratio, c.max, c.runs, c.unended
	out["interrupted"], out["completed"], out["unconfirmed"] = 0, completed, 0
	out["leadMs"], out["waitCount"], out["waitUnconfirmed"], out["waitMs"] = leadMs, waitCount, 0, waitMs
	return out
}

// workTimeline is how long the period ran implementations, in parallel, idle or
// unmeasurable because a run's end was not recorded.
type workTimeline struct {
	runs, unended, max                     int
	active, parallel, idle, gapMs, totalMs int64
}

// workConcurrency measures the concurrent implementation runs over the period.
func workConcurrency(r records, p Period) workTimeline {
	type mark struct {
		At    time.Time
		Delta int
		Gap   bool
	}
	marks := []mark{}
	runs, unended := 0, 0
	for _, s := range spans(r, p.To) {
		if !s.From.Before(p.From) && s.From.Before(p.To) {
			runs++
			if !s.Ended {
				unended++
			}
		}
		from, to := s.From, s.To
		if from.Before(p.From) {
			from = p.From
		}
		if to.After(p.To) {
			to = p.To
		}
		if from.Before(to) {
			marks = append(marks, mark{from, 1, !s.Ended}, mark{to, -1, !s.Ended})
		}
	}
	sort.SliceStable(marks, func(i, j int) bool {
		if marks[i].At.Equal(marks[j].At) {
			return marks[i].Delta < marks[j].Delta
		}
		return marks[i].At.Before(marks[j].At)
	})
	current, max, gaps := 0, 0, 0
	active, parallel, idle, gapMs, totalMs := int64(0), int64(0), int64(0), int64(0), int64(0)
	previous := p.From
	for _, m := range marks {
		duration := m.At.Sub(previous).Milliseconds()
		if gaps > 0 {
			gapMs += duration
		} else if current > 0 {
			totalMs += duration * int64(current)
			active += duration
			if current >= 2 {
				parallel += duration
			}
		} else {
			idle += duration
		}
		if m.Gap {
			gaps += m.Delta
		} else {
			current += m.Delta
		}
		if gaps == 0 && current > max {
			max = current
		}
		previous = m.At
	}
	if previous.Before(p.To) {
		if gaps > 0 {
			gapMs += p.To.Sub(previous).Milliseconds()
		} else {
			idle += p.To.Sub(previous).Milliseconds()
		}
	}
	return workTimeline{runs: runs, unended: unended, max: max, active: active, parallel: parallel, idle: idle, gapMs: gapMs, totalMs: totalMs}
}

// workLeadTimes totals the waits from filing to the first lease and the lead
// times from filing to the first merge within the period.
func workLeadTimes(r records, p Period) (completed, waitCount int, lead, wait int64) {
	firstStart, firstDone := map[int64]bool{}, map[int64]bool{}
	for _, e := range r.Events {
		j := r.Jobs[e.Job]
		within := !e.At.Before(p.From) && e.At.Before(p.To)
		if e.Kind == "leased" && !firstStart[e.Job] {
			firstStart[e.Job] = true
			if within && !e.At.Before(j.Filed) {
				waitCount++
				wait += e.At.Sub(j.Filed).Milliseconds()
			}
		}
		if e.Kind == "state" && e.Data["to"] == "merged" && !firstDone[e.Job] {
			firstDone[e.Job] = true
			if within && !e.At.Before(j.Filed) {
				completed++
				lead += e.At.Sub(j.Filed).Milliseconds()
			}
		}
	}
	return completed, waitCount, lead, wait
}
func (s *Service) Work(ctx context.Context, q Query, repos []string) (map[string]any, error) {
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	r, err := s.read(ctx, q.Project, to)
	if err != nil {
		return nil, err
	}
	var coverage any
	if !r.Oldest.IsZero() {
		coverage = map[string]any{"start": ISO(r.Oldest), "end": ISO(to)}
	}
	before, after := workPeriod(r, q.Before), workPeriod(r, q.After)
	for _, pair := range []struct {
		p   Period
		out map[string]any
	}{{q.Before, before}, {q.After, after}} {
		days := []any{}
		date := pair.p.From.In(JST)
		for at := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, JST); at.Before(pair.p.To); at = at.Add(24 * time.Hour) {
			from, end := at, at.Add(24*time.Hour)
			if from.Before(pair.p.From) {
				from = pair.p.From
			}
			if end.After(pair.p.To) {
				end = pair.p.To
			}
			day := workPeriod(r, Period{from, end})
			day["date"] = at.Format("2006-01-02")
			days = append(days, day)
		}
		pair.out["days"] = days
	}
	var worker bool
	if err = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hosts WHERE heartbeat_at>=now()-interval '15 minutes')").Scan(&worker); err != nil {
		return nil, err
	}
	return map[string]any{"project": q.Project, "projects": repos, "worker": worker, "coverage": coverage, "stoppedSince": nil, "unavailable": []any{}, "before": before, "after": after, "storeLoad": nil}, nil
}

type serverTakeover struct {
	Revision string
	At       time.Time
}

// intakeDeployments keeps the takeover that deployed each intake revision. A
// takeover is a failed switch when the next takeover by another revision is by
// one that already ran before it (deploy-server.sh switched back), so the
// revision is deployed by its first takeover that was not switched back.
func intakeDeployments(takeovers []serverTakeover) []deployment {
	first := map[string]time.Time{}
	for _, t := range takeovers {
		if _, ok := first[t.Revision]; !ok {
			first[t.Revision] = t.At
		}
	}
	deployed := map[string]bool{}
	out := []deployment{}
	for i, t := range takeovers {
		if deployed[t.Revision] {
			continue
		}
		switchedBack := false
		for _, next := range takeovers[i+1:] {
			if next.Revision != t.Revision {
				switchedBack = first[next.Revision].Before(t.At)
				break
			}
		}
		if !switchedBack {
			deployed[t.Revision] = true
			out = append(out, deployment{Repo: "mofua-suki/issue-desk-intake", Commit: t.Revision, Status: "deployed", Finished: sql.NullTime{Time: t.At, Valid: true}})
		}
	}
	return out
}
