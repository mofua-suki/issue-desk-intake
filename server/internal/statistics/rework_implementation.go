package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"sort"
	"time"
)

type implementationRows struct {
	jobs, events, usage []AccountRecord
	unreadable          map[string]bool
	skipped             int
}

func readImplementationRows(ctx context.Context, tx *sql.Tx, repositories []string, all bool) (implementationRows, error) {
	data := implementationRows{}

	jobs, e := readJSONRows(ctx, tx, `SELECT row_to_json(j) FROM jobs j WHERE ($2 OR repo=ANY($1::text[])) ORDER BY id`, repositories, all)
	if e != nil {
		return data, e
	}
	events, e := readJSONRows(ctx, tx, `SELECT row_to_json(e) FROM job_events e JOIN jobs j ON j.id=e.job_id WHERE ($2 OR j.repo=ANY($1::text[])) AND e.kind IN ('leased','state','lease_expired') ORDER BY e.at,e.seq`, repositories, all)
	if e != nil {
		return data, e
	}
	stored, e := tx.QueryContext(ctx, `SELECT u.repo,u.issue,u.json FROM usage_rows u LEFT JOIN jobs j ON j.repo=u.repo AND j.issue=u.issue WHERE u.repo IS NOT NULL AND ($2 OR u.repo=ANY($1::text[])) AND (j.id IS NOT NULL OR u.issue IS NULL) ORDER BY u.at,u.key`, repositories, all)
	if e != nil {
		return data, e
	}
	rows := []AccountRecord{}
	skipped := 0
	unreadable := map[string]bool{}
	for stored.Next() {
		var repo, raw string
		var issue sql.NullInt64
		if e = stored.Scan(&repo, &issue, &raw); e != nil {
			stored.Close()
			return data, e
		}
		var row AccountRecord
		if json.Unmarshal([]byte(raw), &row) != nil || row == nil || row["repo"] != repo || (!issue.Valid || fleet.Number(row["issue"]) != float64(issue.Int64)) || !accountWhole(row["issue"]) || fleet.Number(row["issue"]) < 1 || accountAt(row["at"]).IsZero() {
			skipped++
			unreadable[fmt.Sprintf("%s#%d", repo, issue.Int64)] = true
			continue
		}
		rows = append(rows, row)
	}
	e = stored.Err()
	stored.Close()
	if e != nil {
		return data, e
	}
	data = implementationRows{jobs: jobs, events: events, usage: rows, unreadable: unreadable, skipped: skipped}
	return data, nil
}
func readImplementationBreakdown(ctx context.Context, tx *sql.Tx, repositories []string, period Period, now time.Time) (AccountRecord, error) {
	data, err := readImplementationRows(ctx, tx, repositories, false)
	if err != nil {
		return nil, err
	}
	return data.build(period, now), nil
}

func (data implementationRows) build(period Period, now time.Time) AccountRecord {
	return buildImplementationBreakdown(data.jobs, data.events, data.usage, data.unreadable, period, now, data.skipped)
}

// noRecordBefore preserves the comparison window's shared observation horizon.
func (data implementationRows) noRecordBefore(to time.Time) int {
	recorded := map[string]bool{}
	for _, row := range data.usage {
		if row["step"] == "実装" && accountAt(row["at"]).Before(to) {
			recorded[accountKey(row)] = true
		}
	}
	count := 0
	for _, job := range data.jobs {
		key := accountKey(job)
		if !recorded[key] && !data.unreadable[key] {
			count++
		}
	}
	return count
}

func buildImplementationBreakdown(jobRows, events, usageRows []AccountRecord, unreadable map[string]bool, period Period, now time.Time, skipped int) AccountRecord {
	reasons, unknown := reasonCounts(), unknownCounts()
	daily, days := implementationDays(period)
	issues := []AccountRecord{}
	runs, unknownGenerations, noRecord := 0, 0, 0
	_, usageByIssue := accountGroups(usageRows, accountKey)
	_, eventsByJob := accountGroups(events, func(r AccountRecord) string { return fmt.Sprint(fleet.Number(r["job_id"])) })
	for _, job := range jobRows {
		key := accountKey(job)
		ij := newImplementationJob(job, usageByIssue[key], eventsByJob[fmt.Sprint(fleet.Number(job["id"]))])
		if !ij.recordedBefore(period.To) && !unreadable[key] {
			noRecord++
		}
		jobRuns, counted, unidentified := ij.runs(unreadable[key], period, now)
		for _, run := range jobRuns {
			if run.detail == "inProgress" {
				continue
			}
			runs++
			if run.generation == "unknown" {
				unknownGenerations++
			}
			reason, detail := run.reason, run.detail
			reasons[reason] = reasons[reason].(int) + 1
			day := days[run.end.In(JST).Format("2006-01-02")]
			day["runs"] = day["runs"].(int) + 1
			dayReasons := fleet.Object(day["reasons"])
			dayReasons[reason] = dayReasons[reason].(int) + 1
			if reason == "unknown" {
				unknown[detail] = unknown[detail].(int) + 1
				dayUnknown := fleet.Object(day["unknownDetails"])
				dayUnknown[detail] = dayUnknown[detail].(int) + 1
			}
		}
		if len(jobRuns) > 0 {
			var observed, uc any
			if !unidentified {
				observed = counted
			}
			if accountWhole(job["generation"]) {
				uc = jobs.ImplementationRuns(&jobs.Job{Generation: int64(fleet.Number(job["generation"])), View: fleet.Object(job["view"])})
			}
			issues = append(issues, AccountRecord{"key": key, "uc1Implementations": uc, "implementations": observed})
		}
	}
	out := implementationIssueSummary(issues)
	out["runs"], out["reasons"], out["unknownDetails"], out["days"], out["issues"] = runs, reasons, unknown, daily, issues
	out["unknownGenerations"], out["skipped"], out["noImplementationRecord"] = unknownGenerations, skipped, noRecord
	return out
}

// implementationDays prepares an empty count for each day of the period.
func implementationDays(period Period) ([]AccountRecord, map[string]AccountRecord) {
	daily := []AccountRecord{}
	days := map[string]AccountRecord{}
	d := period.From.In(JST)
	for at := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, JST); at.Before(period.To); at = at.Add(24 * time.Hour) {
		key := at.Format("2006-01-02")
		d := AccountRecord{"day": key, "runs": 0, "reasons": reasonCounts(), "unknownDetails": unknownCounts()}
		days[key] = d
		daily = append(daily, d)
	}
	return daily, days
}

// implementationJob holds one job's usage rows and events for identifying its
// implementation runs.
type implementationJob struct {
	job                    AccountRecord
	issueUsage, events     []AccountRecord
	rows, leases, terminal []AccountRecord
	expires                map[string]time.Time
}

func newImplementationJob(job AccountRecord, issueUsage, events []AccountRecord) *implementationJob {
	rows := []AccountRecord{}
	for _, r := range issueUsage {
		if r["step"] == "実装" {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return accountAt(rows[i]["at"]).Before(accountAt(rows[j]["at"])) })
	leases := []AccountRecord{}
	terminal := []AccountRecord{}
	expires := map[string]time.Time{}
	for _, event := range events {
		if event["kind"] == "leased" {
			leases = append(leases, event)
		}
		d := fleet.Object(event["data"])
		if event["kind"] == "lease_expired" {
			host, epoch, at := fleet.Text(d["leaseHost"]), d["epoch"], accountAt(event["at"])
			if host != "" && accountWhole(epoch) && fleet.Number(epoch) > 0 && !at.IsZero() {
				key := identity(host, epoch)
				if prior := expires[key]; prior.IsZero() || at.Before(prior) {
					expires[key] = at
				}
			}
		}
		if (event["kind"] == "lease_expired" || event["kind"] == "state" && includes([]string{"merged", "held", "failed", "stopped", "closed"}, fleet.Text(d["to"]))) && !includes([]string{"scope_wait", "deletions"}, fleet.Text(d["holdReason"])) {
			terminal = append(terminal, event)
		}
	}
	return &implementationJob{job: job, issueUsage: issueUsage, events: events, rows: rows, leases: leases, terminal: terminal, expires: expires}
}

// recorded reports whether the job has any implementation usage row.
func (ij *implementationJob) recordedBefore(to time.Time) bool {
	for _, row := range ij.rows {
		if accountAt(row["at"]).Before(to) {
			return true
		}
	}
	return false
}

// leaseOf finds the lease a usage row ran under, or -1 when it cannot be identified uniquely.
func (ij *implementationJob) leaseOf(row AccountRecord) int {
	found := -1
	at := accountAt(row["at"])
	for i, lease := range ij.leases {
		if !accountAt(lease["at"]).After(at) {
			found = i
		}
	}
	if found < 0 {
		return -1
	}
	lease := ij.leases[found]
	epoch := fleet.Object(lease["data"])["epoch"]
	if row["host"] != lease["host"] || fleet.Text(row["host"]) == "" || !accountWhole(epoch) || fleet.Number(epoch) < 1 {
		return -1
	}
	if expiry := ij.expires[identity(row["host"], epoch)]; !expiry.IsZero() && at.After(expiry) {
		return -1
	}
	matches := 0
	for _, other := range ij.leases {
		if accountAt(other["at"]).Equal(accountAt(lease["at"])) || fleet.Object(other["data"])["epoch"] == epoch {
			matches++
		}
	}
	if matches != 1 {
		return -1
	}
	return found
}

// runKey identifies the run of a usage row by host, lease epoch and generation.
func (ij *implementationJob) runKey(row AccountRecord) string {
	lease := ij.leaseOf(row)
	if lease < 0 || !accountWhole(row["generation"]) || fleet.Number(row["generation"]) < 1 {
		return "unknown"
	}
	return identity(row["host"], fleet.Object(ij.leases[lease]["data"])["epoch"], row["generation"])
}

// implementationRun is one implementation run that ended within the period and how it ended.
type implementationRun struct {
	generation     string
	end            time.Time
	reason, detail string
}

// runs lists the job's runs that ended within the period. counted is the
// number of runs begun before the period ended, and unidentified tells that
// count is unreliable.
func (ij *implementationJob) runs(unreadable bool, period Period, now time.Time) (runs []implementationRun, counted int, unidentified bool) {
	ordered, groups := accountGroups(ij.rows, ij.runKey)
	sort.SliceStable(ordered, func(i, j int) bool {
		return accountAt(groups[ordered[i]][0]["at"]).Before(accountAt(groups[ordered[j]][0]["at"]))
	})
	unidentified = unreadable
	for _, gen := range ordered {
		if accountAt(groups[gen][0]["at"]).Before(period.To) {
			counted++
			unidentified = unidentified || gen == "unknown"
		}
	}
	for i, gen := range ordered {
		group := groups[gen]
		last := group[len(group)-1]
		end := accountAt(last["at"])
		if end.Before(period.From) || !end.Before(period.To) {
			continue
		}
		reason, detail := ij.classify(ordered, groups, i, now)
		runs = append(runs, implementationRun{generation: gen, end: end, reason: reason, detail: detail})
	}
	return runs, counted, unidentified
}

// classify decides why the i-th run ended, and for an unknown reason, why it is unknown.
func (ij *implementationJob) classify(ordered []string, groups map[string][]AccountRecord, i int, now time.Time) (reason, detail string) {
	gen := ordered[i]
	group := groups[gen]
	last := group[len(group)-1]
	end := accountAt(last["at"])
	next := time.Time{}
	if i+1 < len(ordered) {
		next = accountAt(groups[ordered[i+1]][0]["at"])
	}
	leaseIndex := ij.leaseOf(last)
	lease := AccountRecord(nil)
	if leaseIndex >= 0 {
		lease = ij.leases[leaseIndex]
	}
	event, ambiguous := ij.runTerminal(lease, end, next)
	ambiguous = ambiguous || ij.leaseShared(ordered, groups, gen, leaseIndex)
	record, recorded := AccountRecord(nil), false
	if !ambiguous {
		record, recorded = implementationEndRecord(event, lease)
	}
	d := fleet.Object(event["data"])
	snapshot := AccountRecord(nil)
	if !recorded && !ambiguous && event != nil && accountAt(ij.job["updated_at"]).Equal(accountAt(event["at"])) && ij.job["status"] == d["to"] {
		snapshot = ij.job
	}
	progressed := false
	for _, r := range ij.issueUsage {
		at := accountAt(r["at"])
		progressed = progressed || r["step"] == "独立レビュー" && !at.Before(end) && (next.IsZero() || at.Before(next)) && ij.runKey(r) == gen
	}
	reason = "unknown"
	if gen != "unknown" && !ambiguous {
		reason = classifyImplementation(group, event, snapshot, record, recorded, progressed)
	}
	inProgress := event == nil && !progressed && i == len(ordered)-1 && leaseIndex == len(ij.leases)-1 && leaseIndex >= 0 && ij.leaseRunning(lease, now)
	switch {
	case gen == "unknown" || ambiguous:
		detail = "generationUnidentified"
	case inProgress:
		detail = "inProgress"
	default:
		detail = unknownEndDetail(snapshot, recorded)
	}
	return reason, detail
}

// runTerminal finds the first terminal event of the lease between the run's
// end and the next run, skipping events after a newer lease was taken. It
// also reports whether several events share that first time.
func (ij *implementationJob) runTerminal(lease AccountRecord, end, next time.Time) (AccountRecord, bool) {
	if lease == nil {
		return nil, false
	}
	candidates := []AccountRecord{}
	for _, event := range ij.terminal {
		at := accountAt(event["at"])
		host := event["host"]
		if event["kind"] == "lease_expired" {
			host = fleet.Object(event["data"])["leaseHost"]
		}
		if at.Before(end) || !next.IsZero() && !at.Before(next) || host != lease["host"] || fleet.Object(event["data"])["epoch"] != fleet.Object(lease["data"])["epoch"] {
			continue
		}
		newer := false
		for _, other := range ij.leases {
			t := accountAt(other["at"])
			newer = newer || t.After(accountAt(lease["at"])) && !t.After(at)
		}
		if !newer {
			candidates = append(candidates, event)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	event := candidates[0]
	same := 1
	for _, c := range candidates[1:] {
		if !accountAt(c["at"]).Equal(accountAt(event["at"])) {
			break
		}
		// Repeated sweeps can record queued expiries and then a failure at the
		// same instant. The final expiry is the outcome of that sweep sequence.
		if event["kind"] == "lease_expired" && c["kind"] == "lease_expired" {
			event = c
			continue
		}
		same++
	}
	return event, same > 1
}

// leaseShared reports whether another run on the same lease overlaps this one,
// so the two cannot be told apart.
func (ij *implementationJob) leaseShared(ordered []string, groups map[string][]AccountRecord, gen string, leaseIndex int) bool {
	if leaseIndex < 0 {
		return false
	}
	group := groups[gen]
	end := accountAt(group[len(group)-1]["at"])
	for _, other := range ordered {
		if other == gen {
			continue
		}
		otherRows := groups[other]
		if ij.leaseOf(otherRows[len(otherRows)-1]) == leaseIndex && !accountAt(otherRows[0]["at"]).After(end) && !accountAt(otherRows[len(otherRows)-1]["at"]).Before(accountAt(group[0]["at"])) {
			return true
		}
	}
	return false
}

// leaseRunning reports whether the job still runs on the lease and the lease
// has neither expired nor been recorded as expired.
func (ij *implementationJob) leaseRunning(lease AccountRecord, now time.Time) bool {
	expired := false
	for _, event := range ij.events {
		expired = expired || event["kind"] == "lease_expired" && fleet.Object(event["data"])["epoch"] == fleet.Object(lease["data"])["epoch"]
	}
	return ij.job["status"] == "running" && ij.job["lease_host"] == lease["host"] && ij.job["lease_epoch"] == fleet.Object(lease["data"])["epoch"] && accountAt(ij.job["lease_expires_at"]).After(now) && !expired
}

// unknownEndDetail tells a run whose end has a record or a job snapshot
// (classified but unknown) from one with no record of why it ended.
func unknownEndDetail(snapshot AccountRecord, recorded bool) string {
	view := fleet.Object(snapshot["view"])
	_, usageStop := view["implementationUsageStop"]
	_, deps := view["codexDepsUnavailable"]
	if recorded || snapshot["result"] != nil || usageStop || deps {
		return "unclassified"
	}
	return "reasonNotRecorded"
}

// implementationIssueSummary counts the issues by number of implementations,
// observed and by the UC1 rule, and the issues where they differ.
func implementationIssueSummary(issues []AccountRecord) AccountRecord {
	distribution := func(field string) []AccountRecord {
		counts := map[int]int{}
		for _, i := range issues {
			if i[field] != nil {
				counts[int(fleet.Number(i[field]))]++
			}
		}
		keys := []int{}
		for key := range counts {
			keys = append(keys, key)
		}
		sort.Ints(keys)
		out := []AccountRecord{}
		for _, key := range keys {
			out = append(out, AccountRecord{"implementations": key, "issues": counts[key]})
		}
		return out
	}
	unknownIssues, ucUnknown, mismatch := 0, 0, 0
	for _, i := range issues {
		if i["implementations"] == nil {
			unknownIssues++
		}
		if i["uc1Implementations"] == nil {
			ucUnknown++
		}
		if i["implementations"] != nil && i["uc1Implementations"] != nil && fleet.Number(i["implementations"]) != fleet.Number(i["uc1Implementations"]) {
			mismatch++
		}
	}
	return AccountRecord{"distribution": distribution("implementations"), "unknownIssues": unknownIssues, "uc1Distribution": distribution("uc1Implementations"), "uc1UnknownIssues": ucUnknown, "implementationMismatchIssues": mismatch}
}
