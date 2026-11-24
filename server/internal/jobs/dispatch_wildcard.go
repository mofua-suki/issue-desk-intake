package jobs

import (
	"time"

	"issue-desk-intake/server/internal/fleet"
)

// preventWildcardOvertaking protects an interrupt only while running work is
// its remaining obstacle on at least one assigned host. Probes retain the
// actual running jobs for capacity and preference calculations.
func preventWildcardOvertaking(r *reservation, e dispatchEligibility, queued []*Job, candidate *Job) (bool, error) {
	// Match ordinary reservations: publication resumption must not wait for
	// scope protection on the host that can resume it.
	if r.resumed[candidate.ID] {
		return false, nil
	}
	for _, owner := range queued {
		if owner.ID == candidate.ID || owner.Priority != 1 || candidate.Priority >= owner.Priority || !estimatedWildcard(owner) || !r.overlap(owner, candidate) {
			continue
		}
		for _, host := range r.a.Implementers[owner.Repo] {
			// Publication resumption is host-specific: the host asking Next
			// may resume while another assigned host must wait for overlap.
			if r.resumeAnywhere[owner.ID] || r.resumeOwners[owner.ID] == host {
				continue
			}
			blocked := false
			for _, busy := range r.running {
				if !r.published[busy.ID] && r.overlap(owner, busy) {
					blocked = true
					break
				}
			}
			if !blocked {
				continue
			}
			entry := r.a.Hosts[host]
			if entry == nil {
				continue
			}
			state, exists := e.states[host]
			if !exists || activeDrain(state.Drain, e.at, e.drain) {
				continue
			}
			if _, reported := state.Capabilities["repositories"].([]any); !reported {
				var blocked bool
				err := e.tx.QueryRowContext(e.ctx, `SELECT EXISTS(SELECT 1 FROM job_events ev JOIN jobs j ON j.id=ev.job_id WHERE ev.host=$1 AND ev.kind='handed_back' AND ev.data->>'reason'='adopt_failed' AND ev.at>=$2 AND lower(j.repo)=lower($3))`, host, e.at.Add(-30*time.Minute), owner.Repo).Scan(&blocked)
				if err != nil {
					return false, err
				}
				if blocked {
					continue
				}
			}
			overloaded, err := dispatchOverloaded(e.ctx, e.tx, host, e.at, fleet.Object(fleet.Object(e.a.Output["dispatch"])["loadLimit"]), time.Duration(fleet.Number(fleet.Object(e.a.Output["dispatch"])["leaseMinutes"]))*time.Minute)
			if err != nil {
				return false, err
			}
			if overloaded {
				continue
			}
			probe := e
			probe.host, probe.capacity = host, entry.Max
			decision, err := probe.checkWithOverlap(owner, false, false)
			if err != nil {
				return false, err
			}
			if decision.ready {
				return true, r.record(candidate, owner.ID, false)
			}
		}
	}
	return false, nil
}
