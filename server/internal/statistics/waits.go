package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"log"
	"sort"
	"strings"
	"time"
)

type waitJob struct {
	ID          int64
	Repo        string
	Filed       time.Time
	Scope       []string
	Events      []event
	ScopeEvents []event
	Deps        []int64
	stateIndex  *waitStateIndex
}
type waitState struct{ Status, Host, Hold string }
type waitEntry struct {
	Repo           string
	Duration       *float64
	Parts          map[string]float64
	Missing, Slots bool
}

func waitTransition(e event) (waitState, bool) {
	if e.Kind == "control" && e.Data["command"] != nil {
		return waitState{}, false
	}
	switch e.Kind {
	case "leased":
		return waitState{Status: "running", Host: e.Host}, true
	case "state":
		hold := fleet.Text(e.Data["holdReason"])
		if !oneOf(hold, strings.Fields("review_held question deletions scope_wait")) {
			hold = "unknown"
		}
		return waitState{Status: fleet.Text(e.Data["to"]), Host: e.Host, Hold: hold}, true
	case "closed":
		return waitState{Status: "closed"}, true
	case "scope_wait":
		return waitState{Status: "held", Hold: "scope_wait"}, true
	case "lease_expired", "requeued":
		return waitState{Status: "queued"}, true
	case "control":
		if oneOf(e.Data["action"], strings.Fields("retry resume approve_deletions")) {
			return waitState{Status: "queued"}, true
		}
		if e.Data["action"] == "stop" {
			return waitState{Status: "stopped"}, true
		}
	}
	return waitState{}, false
}

func (s *Service) readWait(ctx context.Context, to time.Time) (_ map[int64]*waitJob, err error) {
	stage := "jobs"
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		logWaitCancellation(stage, err)
	}()
	if err := waitStage(ctx, stage); err != nil {
		return nil, err
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	all, e := readWaitJobs(ctx, tx, to)
	if e != nil {
		return nil, e
	}
	stage = "events"
	if err := waitStage(ctx, stage); err != nil {
		return nil, err
	}
	if e = readWaitEvents(ctx, tx, to, all); e != nil {
		return nil, e
	}
	stage = "dependencies"
	if err := waitStage(ctx, stage); err != nil {
		return nil, err
	}
	if e = readWaitDeps(ctx, tx, all); e != nil {
		return nil, e
	}
	return all, tx.Commit()
}

// readWaitJobs reads the jobs filed before to with their scopes.
func readWaitJobs(ctx context.Context, tx *sql.Tx, to time.Time) (map[int64]*waitJob, error) {
	rows, e := tx.QueryContext(ctx, "SELECT id,repo,filed_at,array_to_json(scope)FROM jobs WHERE filed_at<$1", to)
	if e != nil {
		return nil, e
	}
	all := map[int64]*waitJob{}
	for rows.Next() {
		j := &waitJob{}
		var raw []byte
		if e = rows.Scan(&j.ID, &j.Repo, &j.Filed, &raw); e != nil {
			rows.Close()
			return nil, e
		}
		if e = json.Unmarshal(raw, &j.Scope); e != nil {
			rows.Close()
			return nil, e
		}
		all[j.ID] = j
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	return all, nil
}

// readWaitEvents adds each job's events before to and all its scope events.
func readWaitEvents(ctx context.Context, tx *sql.Tx, to time.Time, all map[int64]*waitJob) error {
	rows, e := tx.QueryContext(ctx, "SELECT seq,job_id,coalesce(host,''),kind,at,data FROM job_events WHERE at<$1 OR kind IN ('scope_narrowed','scope_widened','scope_wait') ORDER BY at,seq", to)
	if e != nil {
		return e
	}
	for rows.Next() {
		v := event{}
		var raw []byte
		if e = rows.Scan(&v.Seq, &v.Job, &v.Host, &v.Kind, &v.At, &raw); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(raw, &v.Data); e != nil {
			rows.Close()
			return e
		}
		if j := all[v.Job]; j != nil {
			if v.At.Before(to) {
				j.Events = append(j.Events, v)
			}
			if v.Kind == "scope_narrowed" || v.Kind == "scope_widened" || v.Kind == "scope_wait" {
				j.ScopeEvents = append(j.ScopeEvents, v)
			}
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	return nil
}

// readWaitDeps adds each job's dependencies.
func readWaitDeps(ctx context.Context, tx *sql.Tx, all map[int64]*waitJob) error {
	rows, e := tx.QueryContext(ctx, "SELECT job_id,depends_on FROM job_deps")
	if e != nil {
		return e
	}
	for rows.Next() {
		var id, dep int64
		if e = rows.Scan(&id, &dep); e != nil {
			rows.Close()
			return e
		}
		if j := all[id]; j != nil {
			j.Deps = append(j.Deps, dep)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	return nil
}

func logWaitCancellation(stage string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		log.Printf("stats waits stage=%s error=%v", stage, context.DeadlineExceeded)
	} else if errors.Is(err, context.Canceled) {
		log.Printf("stats waits stage=%s error=%v", stage, context.Canceled)
	}
}
func waitAdd(entry *waitEntry, cause string, from, to time.Time) {
	if !to.After(from) {
		return
	}
	entry.Parts[cause] += float64(to.Sub(from)) / float64(time.Millisecond)
	if cause == "unknown" || cause == "held:unknown" {
		entry.Missing = true
	}
}

func prepareWait(ctx context.Context, all map[int64]*waitJob, cache *waitScopeCache) error {
	for _, j := range all {
		if j.stateIndex == nil {
			index, err := buildWaitStateIndex(ctx, j.Events)
			if err != nil {
				return err
			}
			j.stateIndex = index
		}
	}
	if !cache.legacyCuts && !cache.timelineReady {
		var err error
		cache.timeline, err = waitTimeline(ctx, all)
		if err != nil {
			return err
		}
		cache.timelineReady = true
	}
	return nil
}

func waitEntries(ctx context.Context, all map[int64]*waitJob, p Period, project string, a *fleet.Assignments, cache *waitScopeCache) ([]waitEntry, []waitEntry, error) {
	starts, publishes := []waitEntry{}, []waitEntry{}
	if err := prepareWait(ctx, all, cache); err != nil {
		return nil, nil, err
	}
	for _, j := range all {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if project != "" && j.Repo != project {
			continue
		}
		leased, pr, merged, err := waitMilestones(ctx, j)
		if err != nil {
			return nil, nil, err
		}
		for _, mode := range []string{"start", "publish"} {
			begin, end := j.Filed, leased
			if mode == "publish" {
				begin, end = pr, merged
			}
			placed := end
			if placed.IsZero() {
				placed = begin
			}
			if placed.IsZero() {
				placed = j.Filed
			}
			if placed.Before(p.From) || !placed.Before(p.To) {
				continue
			}
			entry := waitEntry{Repo: j.Repo, Parts: map[string]float64{}, Slots: implementerSlots(a, j.Repo)}
			valid := !begin.IsZero() && !end.IsZero() && !end.Before(begin)
			entry.Missing = !valid
			if valid {
				duration := float64(end.Sub(begin)) / float64(time.Millisecond)
				entry.Duration = &duration
				cuts, err := waitCuts(ctx, mode, j, all, cache, begin, end)
				if err != nil {
					return nil, nil, err
				}
				sort.Slice(cuts, func(i, k int) bool { return cuts[i].Before(cuts[k]) })
				for i := 0; i+1 < len(cuts); i++ {
					if err := ctx.Err(); err != nil {
						return nil, nil, err
					}
					at := cuts[i]
					cause := "other"
					if mode == "start" {
						countWaitStartInterval(ctx)
						var err error
						cause, err = waitStartCause(ctx, cache, j, all, at, a, entry.Slots)
						if err != nil {
							return nil, nil, err
						}
					} else {
						var err error
						cause, err = waitPublishCause(ctx, j, at)
						if err != nil {
							return nil, nil, err
						}
					}
					waitAdd(&entry, cause, at, cuts[i+1])
				}
			}
			if mode == "start" {
				starts = append(starts, entry)
			} else {
				publishes = append(publishes, entry)
			}
		}
	}
	return starts, publishes, nil
}

// waitMilestones finds when the job was first leased, its PR created and merged.
func waitMilestones(ctx context.Context, j *waitJob) (leased, pr, merged time.Time, _ error) {
	for _, e := range j.Events {
		countWaitEvent(ctx)
		if err := ctx.Err(); err != nil {
			return time.Time{}, time.Time{}, time.Time{}, err
		}
		if e.Kind == "leased" && leased.IsZero() {
			leased = e.At
		}
		if e.Kind == "pr_created" && pr.IsZero() {
			pr = e.At
		}
		if e.Kind == "state" && e.Data["to"] == "merged" && merged.IsZero() {
			merged = e.At
		}
	}
	return leased, pr, merged, nil
}

// implementerSlots tells whether every implementer of the repository can take work.
func implementerSlots(a *fleet.Assignments, repo string) bool {
	slots := false
	if a != nil && len(a.Implementers[repo]) > 0 {
		slots = true
		for _, host := range a.Implementers[repo] {
			if a.Hosts[host] == nil || a.Hosts[host].Max <= 0 {
				slots = false
			}
		}
	}
	return slots
}

// waitCuts lists the times within the wait where its cause may change, unsorted.
func waitCuts(ctx context.Context, mode string, j *waitJob, all map[int64]*waitJob, cache *waitScopeCache, begin, end time.Time) ([]time.Time, error) {
	cuts := []time.Time{begin, end}
	if mode == "start" && !cache.legacyCuts {
		cuts = append(cuts, waitTimelineCuts(cache.timeline, begin, end)...)
	} else if mode == "start" {
		for _, other := range all {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if other.Filed.After(begin) && other.Filed.Before(end) {
				cuts = append(cuts, other.Filed)
			}
			for _, e := range other.Events {
				countWaitEvent(ctx)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if _, ok := waitTransition(e); (ok || e.Kind == "scope_narrowed" || e.Kind == "scope_widened") && e.At.After(begin) && e.At.Before(end) {
					cuts = append(cuts, e.At)
				}
			}
		}
	} else {
		for _, e := range j.Events {
			countWaitEvent(ctx)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if e.At.After(begin) && e.At.Before(end) {
				cuts = append(cuts, e.At)
			}
		}
	}
	return cuts, nil
}

// waitPublishCause attributes the publish wait at the time to review, a hold or an unknown state.
func waitPublishCause(ctx context.Context, j *waitJob, at time.Time) (string, error) {
	cause := "other"
	state, err := waitStateAt(ctx, j, at)
	if err != nil {
		return "", err
	}
	last, next, futureEnds := j.stateIndex.reviewAt(ctx, at)
	if last == "step_start" && futureEnds > 0 {
		cause = "review"
	} else if state.Status == "held" {
		cause = "held:" + state.Hold
	} else if state.Status == "unknown" || last == "step_start" || last == "" && next == "step_end" {
		cause = "unknown"
	}
	return cause, nil
}

func waitSummary(entries []waitEntry, names []string) map[string]any {
	values := []float64{}
	missing := 0
	slots := true
	for _, e := range entries {
		if e.Duration != nil {
			values = append(values, *e.Duration)
		}
		if e.Missing {
			missing++
		}
		slots = slots && e.Slots
	}
	sort.Float64s(values)
	var median, p90 any
	total := float64(0)
	for _, v := range values {
		total += v
	}
	if len(values) > 0 {
		median = (values[(len(values)-1)/2] + values[len(values)/2]) / 2
		index := (len(values)*9+9)/10 - 1
		p90 = values[index]
	}
	causes := map[string]any{}
	for _, name := range names {
		measurable := name != "slotFull" || slots
		ms := float64(0)
		count := 0
		for _, e := range entries {
			ms += e.Parts[name]
			if e.Parts[name] > 0 {
				count++
			}
		}
		part := map[string]any{"ms": nil, "share": nil, "jobs": nil}
		if measurable {
			part["ms"], part["jobs"] = ms, count
			if total > 0 {
				part["share"] = ms / total
			}
		} else {
			part["reason"] = "担当PCの枠を取得できません。"
		}
		causes[name] = part
	}
	return map[string]any{"count": len(entries), "missing": missing, "medianMs": median, "p90Ms": p90, "causes": causes}
}
func waitReport(entries []waitEntry, names []string) map[string]any {
	out := waitSummary(entries, names)
	repos := map[string][]waitEntry{}
	for _, e := range entries {
		repos[e.Repo] = append(repos[e.Repo], e)
	}
	keys := []string{}
	for key := range repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	byRepo := []any{}
	for _, key := range keys {
		row := waitSummary(repos[key], names)
		row["repo"] = key
		byRepo = append(byRepo, row)
	}
	out["byRepo"] = byRepo
	return out
}
func (s *Service) Waits(ctx context.Context, q Query) (any, error) {
	return s.waitsWithLegacyCuts(ctx, q, false)
}

func (s *Service) waitsWithLegacyCuts(ctx context.Context, q Query, legacy bool) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	to := q.Before.To
	if q.After.To.After(to) {
		to = q.After.To
	}
	all, e := s.readWait(ctx, to)
	if e != nil {
		return nil, e
	}
	return s.calculateWaits(ctx, q, all, legacy)
}

func (s *Service) calculateWaits(ctx context.Context, q Query, all map[int64]*waitJob, legacy bool) (_ any, err error) {
	stage := "timeline"
	defer func() { logWaitCancellation(stage, err) }()
	cache := newWaitScopeCache(s.Assignments)
	cache.legacyCuts = legacy
	if err := waitStage(ctx, stage); err != nil {
		return nil, err
	}
	if err := prepareWait(ctx, all, cache); err != nil {
		return nil, err
	}
	period := func(p Period, name string) (any, error) {
		stage = name
		if err := waitStage(ctx, stage); err != nil {
			return nil, err
		}
		start, publish, err := waitEntries(ctx, all, p, q.Project, s.Assignments, cache)
		if err != nil {
			return nil, err
		}
		holds := map[string]bool{}
		for _, e := range publish {
			for key := range e.Parts {
				if strings.HasPrefix(key, "held:") {
					holds[key] = true
				}
			}
		}
		names := []string{}
		for key := range holds {
			names = append(names, key)
		}
		sort.Strings(names)
		names = append(append([]string{"review"}, names...), "other", "unknown")
		stage = "summary"
		if err := waitStage(ctx, stage); err != nil {
			return nil, err
		}
		out := p.Dates()
		out["startWait"], out["publishWait"] = waitReport(start, strings.Fields("dependency scopeOverlap slotFull idle unknown")), waitReport(publish, names)
		return out, ctx.Err()
	}
	before, err := period(q.Before, "before")
	if err != nil {
		return nil, err
	}
	after, err := period(q.After, "after")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"project": q.Project, "unavailable": []any{}, "skipped": 0, "before": before, "after": after, "notes": []string{"配布停止・版ずれ・確認周期・その他の履歴はないため、空きがあるのに未着手（idle）へまとめます。", "担当表は現在値です。scope_narrowedの前後は履歴を使い、過去の枠・担当変更と最初の狭小化以前のwidened前の範囲は近似です。再キュー後の再待ちは含めません。"}}, nil
}
