// Package contract defines the wire version and request/error boundaries.
// It does not implement job dispatch, authentication, or persistence.
package contract

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

const (
	Header  = "x-issue-desk-contract"
	Version = "4cd4625bbadfe9da0aa2a3f0af5ead6a3c8b12da3ef27b3d1f751c2ca9759213"
)

// Recovery describes what a caller may do after an operation fails.
type Recovery struct {
	Action   string `json:"action"`
	Guidance string `json:"guidance"`
}

//go:embed recovery.json
var recoveryJSON []byte

var recoveries = func() map[string]Recovery {
	var result map[string]Recovery
	if err := json.Unmarshal(recoveryJSON, &result); err != nil {
		panic("invalid embedded contract recovery data")
	}
	return result
}()

// Problem is the existing HTTP error envelope. Callers must provide a public,
// redacted message; underlying errors and request bodies must not be exposed.
type Problem struct {
	Status          int      `json:"-"`
	Message         string   `json:"error"`
	Code            string   `json:"code"`
	Recovery        Recovery `json:"recovery"`
	ContractVersion string   `json:"contractVersion"`
	Retryable       string   `json:"retryable"`
	SafeToChange    bool     `json:"safeToChange"`
}

func (p *Problem) Error() string { return p.Message }

// NewProblem conservatively preserves the original send ID: every problem is
// retryable with the same request and is never marked safe to change.
func NewProblem(status int, message, code string) *Problem {
	recoveryCode := code
	if recoveryCode == "" {
		if status == 502 || status == 503 || status == 504 {
			recoveryCode = "TEMPORARY_UNAVAILABLE"
		} else {
			recoveryCode = "REQUEST_REJECTED"
		}
	}
	recovery, ok := recoveries[recoveryCode]
	if !ok {
		recovery = recoveries["REQUEST_REJECTED"]
	}
	if code == "" {
		code = recoveryCode
	}
	return &Problem{
		Status: status, Message: message, Code: code, Recovery: recovery,
		ContractVersion: Version, Retryable: "same-request",
	}
}

// WriteProblem emits the contract error envelope without leaking Go errors.
func WriteProblem(w http.ResponseWriter, problem *Problem) {
	w.Header().Set(Header, Version)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(problem)
}

// NotAcceptedHeader marks a 503 that was returned before the request reached any
// business handler. Nothing was read or saved, so a client may resend the same
// request (including a POST) after Retry-After.
const NotAcceptedHeader = "X-Issue-Desk-Not-Accepted"

func WriteNotAccepted(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set(NotAcceptedHeader, "1")
	WriteProblem(w, NewProblem(http.StatusServiceUnavailable, message, ""))
}
