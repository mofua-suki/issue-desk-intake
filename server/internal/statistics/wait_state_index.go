package statistics

import (
	"context"
	"sort"
	"time"
)

type waitIndexedTransition struct {
	at    time.Time
	order int
	state waitState
}

type waitStateIndex struct {
	transitions []waitIndexedTransition
	reviews     []waitReviewPoint
}

type waitReviewPoint struct {
	at            time.Time
	order         int
	kind          string
	lastKind      string
	nextKind      string
	remainingEnds int
}

func buildWaitStateIndex(ctx context.Context, events []event) (*waitStateIndex, error) {
	index := &waitStateIndex{}
	for n, e := range events {
		countWaitEvent(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if state, ok := waitTransition(e); ok {
			index.transitions = append(index.transitions, waitIndexedTransition{e.At, n, state})
		}
		if (e.Kind == "step_start" || e.Kind == "step_end") && e.Data["label"] == "レビュー" {
			index.reviews = append(index.reviews, waitReviewPoint{at: e.At, order: n, kind: e.Kind})
		}
	}
	sort.Slice(index.transitions, func(i, j int) bool { return index.transitions[i].at.Before(index.transitions[j].at) })
	// The original scan chooses the last matching event in input order, even
	// when events are not chronological. Keep that rule for every prefix.
	var latest waitIndexedTransition
	for i := range index.transitions {
		if i == 0 || index.transitions[i].order > latest.order {
			latest = index.transitions[i]
		}
		index.transitions[i].state = latest.state
	}
	sort.Slice(index.reviews, func(i, j int) bool { return index.reviews[i].at.Before(index.reviews[j].at) })
	lastOrder := -1
	lastKind := ""
	for i := range index.reviews {
		p := &index.reviews[i]
		if p.order > lastOrder {
			lastOrder, lastKind = p.order, p.kind
		}
		p.lastKind = lastKind
	}
	nextOrder := len(events)
	nextKind, ends := "", 0
	for i := len(index.reviews) - 1; i >= 0; i-- {
		p := &index.reviews[i]
		if p.order < nextOrder {
			nextOrder, nextKind = p.order, p.kind
		}
		if p.kind == "step_end" {
			ends++
		}
		p.nextKind, p.remainingEnds = nextKind, ends
	}
	return index, ctx.Err()
}

func (index *waitStateIndex) reviewAt(ctx context.Context, at time.Time) (last, next string, futureEnds int) {
	n := sort.Search(len(index.reviews), func(i int) bool { countWaitEvent(ctx); return index.reviews[i].at.After(at) })
	if n > 0 {
		countWaitEvent(ctx)
		last = index.reviews[n-1].lastKind
	}
	if n < len(index.reviews) {
		countWaitEvent(ctx)
		next, futureEnds = index.reviews[n].nextKind, index.reviews[n].remainingEnds
	}
	return
}

func (index *waitStateIndex) at(ctx context.Context, filed, at time.Time) waitState {
	n := sort.Search(len(index.transitions), func(i int) bool { countWaitEvent(ctx); return index.transitions[i].at.After(at) })
	if n > 0 {
		countWaitEvent(ctx)
		return index.transitions[n-1].state
	}
	if len(index.transitions) > 0 && !filed.After(at) {
		return waitState{Status: "absent"}
	}
	return waitState{Status: "unknown"}
}
