package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

type dispatchSkipCacheKey struct{}
type dispatchSkipCache map[int64][]map[string]any

func loadDispatchSkipCache(ctx context.Context, tx *sql.Tx, jobs []*Job, at time.Time) (context.Context, error) {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT job_id,data FROM job_events WHERE job_id=ANY($1::bigint[]) AND kind='dispatch_skip' AND at>$2`, ids, at.Add(-5*time.Minute))
	if err != nil {
		return ctx, err
	}
	defer rows.Close()
	cache := dispatchSkipCache{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return ctx, err
		}
		var data map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&data); err != nil {
			return ctx, err
		}
		cache[id] = append(cache[id], data)
	}
	if err = rows.Err(); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, dispatchSkipCacheKey{}, cache), nil
}

func sameSkip(old, data map[string]any, host string) bool {
	reason := data["reason"].(string)
	if old["reason"] != reason {
		return false
	}
	if reason == "scope_reserved" {
		return fmt.Sprint(old["by"]) == jsonNumber(data["by"].(int64)) && (fmt.Sprint(old["release"]) == "true") == (data["release"] == true)
	}
	if old["host"] != host {
		return false
	}
	if reason == "overlap" {
		return fmt.Sprint(old["by"]) == jsonNumber(data["by"].(int64))
	}
	if reason == "preference" {
		return old["preferredHost"] == data["preferredHost"]
	}
	return true
}

// recordDispatchSkip keeps the reservation key compatible with historic events.
func recordDispatchSkip(ctx context.Context, tx *sql.Tx, job *Job, host string, data map[string]any, at time.Time) error {
	if cache, ok := ctx.Value(dispatchSkipCacheKey{}).(dispatchSkipCache); ok {
		for _, old := range cache[job.ID] {
			if sameSkip(old, data, host) {
				return nil
			}
		}
		data["host"] = host
		if err := addEvent(ctx, tx, job.ID, host, "dispatch_skip", data, at); err != nil {
			return err
		}
		// JSON round trip preserves the database representation of numeric keys.
		body, err := json.Marshal(data)
		if err != nil {
			return err
		}
		var saved map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err = decoder.Decode(&saved); err != nil {
			return err
		}
		cache[job.ID] = append(cache[job.ID], saved)
		return nil
	}
	reason := data["reason"].(string)
	query := `SELECT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1 AND kind='dispatch_skip' AND data->>'reason'=$2 AND at>$3`
	args := []any{job.ID, reason, at.Add(-5 * time.Minute)}
	if reason == "scope_reserved" {
		query += ` AND data->>'by'=$4 AND coalesce(data->>'release','false')=$5`
		args = append(args, jsonNumber(data["by"].(int64)), transferJSON(data["release"] == true))
	} else {
		query += ` AND data->>'host'=$4`
		args = append(args, host)
		if reason == "overlap" {
			query += ` AND data->>'by'=$5`
			args = append(args, jsonNumber(data["by"].(int64)))
		}
		if reason == "preference" {
			query += ` AND data->>'preferredHost'=$5`
			args = append(args, data["preferredHost"])
		}
	}
	query += `)`
	var recent bool
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&recent); err != nil || recent {
		return err
	}
	data["host"] = host
	return addEvent(ctx, tx, job.ID, host, "dispatch_skip", data, at)
}

func slotData(host, repo string, capacity int, states map[string]hostState, running []*Job, a *fleet.Assignments, early bool) map[string]any {
	hostRunning, repoRunning := 0, 0
	for _, job := range running {
		if job.LeaseHost != nil && *job.LeaseHost == host {
			hostRunning++
			if strings.EqualFold(job.Repo, repo) {
				repoRunning++
			}
		}
	}
	hostLimit := min(a.Hosts[host].Max, capacity)
	if !early && states[host].Capacity >= 0 {
		hostLimit = min(hostLimit, states[host].Capacity)
	}
	repoLimit := a.RepoLimit(host, repo)
	slots := max(0, min(hostLimit-hostRunning, repoLimit-repoRunning))
	return map[string]any{"reason": "no_slot", "slots": slots, "hostRunning": hostRunning, "hostLimit": hostLimit, "repoRunning": repoRunning, "repoLimit": repoLimit}
}

// Probing reservation eligibility must not start an offer or write dispatch history.
func dispatchPreferenceDecision(ctx context.Context, tx *sql.Tx, job *Job, host string, states map[string]hostState, running []*Job, a *fleet.Assignments, at time.Time, record bool, compatible func(hostState) string) (bool, string, error) {
	preference := time.Duration(fleet.Number(fleet.Object(a.Output["dispatch"])["preferenceSeconds"])) * time.Second
	candidates := []string{}
	for _, candidate := range a.Implementers[job.Repo] {
		// A host refused by the artifact guard must not hold the preference wait.
		if compatible(states[candidate]) == "" && availableHost(candidate, job.Repo, states, running, a, at) && artifactDispatchable(job, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	// Retry increments generation; control actions (including resume, instruction and
	// priority changes), reports, lease expiry and scope-wait release update
	// updated_at. Each starts a new preference interval on the next real offer.
	// Reservation age has its own contract and survives lease expiry. Candidate
	// availability and ranking must not change this interval. Historic signatures
	// start one new interval when first encountered.
	signature := fleet.Digest([]any{job.Generation, job.UpdatedAt})
	sort.SliceStable(candidates, func(i, j int) bool {
		return availableSlots(candidates[i], job.Repo, states, running, a, at) > availableSlots(candidates[j], job.Repo, states, running, a, at)
	})
	rank := -1
	for i, candidate := range candidates {
		if candidate == host {
			rank = i
		}
	}
	if rank < 0 {
		return false, "", nil
	}
	var previousAt time.Time
	var data []byte
	err := tx.QueryRowContext(ctx, "SELECT at,data FROM job_events WHERE job_id=$1 AND kind='dispatch_offer' ORDER BY seq DESC LIMIT 1", job.ID).Scan(&previousAt, &data)
	if err != nil && err != sql.ErrNoRows {
		return false, "", err
	}
	var previous map[string]any
	if len(data) > 0 {
		if err = json.Unmarshal(data, &previous); err != nil {
			return false, "", err
		}
	}
	if previous["signature"] != signature {
		previousAt = at
		if record {
			if err = addEvent(ctx, tx, job.ID, "", "dispatch_offer", map[string]any{"signature": signature}, at); err != nil {
				return false, "", err
			}
		}
	}
	return at.Sub(previousAt) >= time.Duration(rank)*preference, candidates[0], nil
}
