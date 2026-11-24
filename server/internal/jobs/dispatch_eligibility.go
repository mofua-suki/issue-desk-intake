package jobs

import (
	"context"
	"database/sql"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
)

// A cycle's candidate and reservation owner share every dispatch gate. Probes
// only read; the caller records skips and leases only the selected candidate.
type dispatchEligibility struct {
	ctx         context.Context
	tx          *sql.Tx
	host        string
	capacity    int
	states      map[string]hostState
	running     []*Job
	a           *fleet.Assignments
	at          time.Time
	drain       time.Duration
	explicit    map[int64]bool
	reservation *reservation
}

type dispatchDecision struct {
	ready  bool
	reason string
	skip   map[string]any
}

func (e dispatchEligibility) check(job *Job, recordOffer bool) (dispatchDecision, error) {
	return e.checkWithOverlap(job, recordOffer, true)
}

func (e dispatchEligibility) checkWithOverlap(job *Job, recordOffer, checkOverlap bool) (dispatchDecision, error) {
	if finalApprovalLeaseLive(job, e.host, e.at) {
		// The previous worker on this PC has not yet seen its lease end.
		return dispatchDecision{}, nil
	}
	ready, ok := e.explicit[job.ID]
	var err error
	if !ok {
		ready, err = explicitDependenciesReady(e.ctx, e.tx, job)
	}
	if err != nil {
		return dispatchDecision{}, err
	}
	if !ready {
		return dispatchDecision{skip: map[string]any{"reason": "dependency"}}, nil
	}
	own := e.states[e.host]
	plan, err := workplan.Parse(job.View["executionPlan"])
	if err != nil {
		return dispatchDecision{skip: map[string]any{"reason": "execution_plan"}}, nil
	}
	compatible := func(state hostState) string { return executionPlanIncompatibility(plan, state) }
	if reason := compatible(own); reason != "" {
		return dispatchDecision{skip: map[string]any{"reason": reason}}, nil
	}

	if !availableHost(e.host, job.Repo, e.states, e.running, e.a, e.at) {
		data := slotData(e.host, job.Repo, e.capacity, e.states, e.running, e.a, false)
		if data["slots"].(int) == 0 && len(fleet.List(own.Hold["reasons"])) == 0 && e.at.Sub(own.Heartbeat) < heartbeatFreshness && artifactDispatchable(job, e.host) {
			return dispatchDecision{skip: data}, nil
		}
		return dispatchDecision{}, nil
	}
	if !artifactDispatchable(job, e.host) {
		return dispatchDecision{}, nil
	}
	preferred, permits, reason := continuationDecision(job, e.host, e.states, e.a, e.at, e.drain, compatible)
	if !permits {
		return dispatchDecision{skip: map[string]any{"reason": "preference", "preferredHost": fleet.Text(fleet.Object(job.View["continuation"])["host"])}}, nil
	}
	for _, busy := range e.running {
		if checkOverlap && e.reservation.busyOverlap(job, busy) {
			return dispatchDecision{skip: map[string]any{"reason": "overlap", "by": busy.ID}}, nil
		}
	}
	if !preferred {
		var preferredHost string
		permits, preferredHost, err = dispatchPreferenceDecision(e.ctx, e.tx, job, e.host, e.states, e.running, e.a, e.at, recordOffer, compatible)
		if err != nil {
			return dispatchDecision{}, err
		}
		if !permits {
			if preferredHost != "" {
				return dispatchDecision{skip: map[string]any{"reason": "preference", "preferredHost": preferredHost}}, nil
			}
			return dispatchDecision{}, nil
		}
	}
	return dispatchDecision{ready: true, reason: reason}, nil
}

// executionPlanIncompatibility applies the same measured capability gate to
// the requesting host and each host that could hold a preference.
func executionPlanIncompatibility(plan *workplan.Plan, state hostState) string {
	if plan == nil {
		return ""
	}
	if fleet.Number(state.Capabilities["executionPlanVersion"]) != 1 {
		return "execution_protocol"
	}
	environment := fleet.Object(state.Capabilities["environment"])
	for _, r := range plan.Requirements {
		if r.EvidenceKind == "fixed_test" {
			for _, capability := range r.Capabilities {
				if environment[capability] != true {
					return "capability"
				}
			}
		}
	}
	return ""
}
