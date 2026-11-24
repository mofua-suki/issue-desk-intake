package ai

import (
	"context"
	"errors"
	"time"
)

type Observation struct {
	DiagnosticKind string    `json:"diagnosticKind"`
	DiagnosticID   string    `json:"diagnosticId"`
	Effort         string    `json:"effort"`
	Step           string    `json:"step"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt"`
	Outcome        string    `json:"outcome"`
	Cause          string    `json:"cause"`
	CauseKey       string    `json:"causeKey"`
}

func observation(request Request, started time.Time, err error) Observation {
	o := Observation{Step: request.Step, Provider: request.Provider, Model: request.Model, StartedAt: started, FinishedAt: time.Now(), Outcome: "success"}
	if err == nil {
		return o
	}
	o.Outcome, o.Cause, o.CauseKey = "failed", "execution", "execution"
	var failure *Failure
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.Canceled):
		o.Outcome, o.Cause, o.CauseKey = "canceled", "intentional_stop", "canceled"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout():
		o.Cause, o.CauseKey = "timeout", "timeout"
	case errors.As(err, &failure):
		o.Cause, o.CauseKey = failure.Kind, failure.Kind
		if failure.Kind == "input_size" || failure.Kind == "schema" {
			o.Cause, o.CauseKey = "input_invalid", "input_invalid"
		}
	}
	return o
}
