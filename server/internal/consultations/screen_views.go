package consultations

import (
	"context"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/images"
	"log"
	"strings"
	"sync"
	"time"
)

type screenInvalidLogState struct {
	sync.Mutex
	last time.Time
}

const screenInvalidKeyLimit = 5

// DraftScreenViews uses the already-read screen snapshot for family action
// checks, avoiding repeated full-record reads for every action and family row.
func (s *Service) DraftScreenViews(entries []DraftFields, p auth.Principal, origin string) []Record {
	byID := make(map[string]Record, len(entries))
	invalid := 0
	invalidKeys := make([]string, 0, screenInvalidKeyLimit)
	for _, entry := range entries {
		d := entry.Record
		id, ok := d["id"].(string)
		if !ok || id == "" || entry.Key != id || byID[id] != nil {
			invalid++
			if len(invalidKeys) < screenInvalidKeyLimit {
				invalidKeys = append(invalidKeys, entry.Key)
			}
			continue
		}
		byID[id] = d
	}
	if invalid > 0 {
		s.screenInvalidLog.Lock()
		if time.Since(s.screenInvalidLog.last) >= time.Minute {
			log.Printf("consultations: draft screen snapshot identifiers invalid; using family projections (invalid rows=%d, first DB keys=%q)", invalid, invalidKeys)
			s.screenInvalidLog.last = time.Now()
		}
		s.screenInvalidLog.Unlock()
	}
	var fallback func(Record, string) string
	if invalid > 0 {
		drafts := make([]Record, len(entries))
		for i, entry := range entries {
			drafts[i] = entry.Record
		}
		fallback = s.familyListViews(context.Background(), drafts)
	}
	family := s.familyRefusalIn(byID)
	out := make([]Record, len(entries))
	for i, entry := range entries {
		d := entry.Record
		if invalid > 0 {
			out[i] = s.draftView(d, p, origin, func(d Record, action string) string { return s.refusalWithFamily(d, action, fallback) })
			delete(out[i], "revision")
			delete(out[i], "submissionRequest")
			continue
		}
		out[i] = s.draftView(d, p, origin, func(d Record, action string) string { return s.refusalWithFamily(d, action, family) })
		delete(out[i], "revision")
		delete(out[i], "submissionRequest")
	}
	return out
}

func (s *Service) DraftView(d Record, p auth.Principal, origin string) Record {
	return s.draftView(d, p, origin, s.refusal)
}

func (s *Service) draftView(d Record, p auth.Principal, origin string, refusal func(Record, string) string) Record {
	out := Record(images.Present(d, origin))
	// A saved thinking status is not evidence that this process still owns a turn.
	busy := s.busy(d)
	out["executionActive"] = busy && text(d["run"]) != "" && d["queued"] != true && d["retryAt"] == nil
	out["executionScheduled"] = busy && (d["queued"] == true || d["retryAt"] != nil) || s.acceptedSubmissionScheduled(d)

	// Only the response is normalized; stored legacy records are never migrated on read.
	if d["status"] == "stalled" {
		cause, present := d["stallCause"]
		if !present {
			switch {
			case strings.HasPrefix(text(d["error"]), "相談を停止しました: "):
				out["stallCause"] = "explicit"
			case text(d["error"]) == "24時間以内にAIの応答を返せなかったため、自動処理を停止しました。":
				out["stallCause"] = "timeout"
			default:
				delete(out, "stallCause")
			}
		} else if cause != "explicit" && cause != "timeout" {
			delete(out, "stallCause")
		}
	} else {
		delete(out, "stallCause")
	}
	delete(out, "owner")
	delete(out, "filingCheckCalls")
	if summary := filingCheckSummary(d); summary != nil {
		out["filingCheckSummary"] = summary
	}
	for key := range out {
		if strings.HasPrefix(key, "_") {
			delete(out, key)
		}
	}
	actions := []string{}
	for _, action := range []string{"chat", "retry", "continue_budget", "submit", "resubmit", "comment", "finish", "edit", "todo", "discard", "proxy_undo", "stop"} {
		if action == "stop" && auth.Authorize(p, draftStopPolicy) != nil {
			continue
		}
		if action == "proxy_undo" && auth.Authorize(p, auth.Policy{Screen: true, PC: true, RequireDelegation: true}) != nil {
			continue
		}
		if action == "retry" && retryHiddenForMeasurement(d) {
			continue
		}
		if refusal(d, action) == "" {
			actions = append(actions, action)
		}
	}
	out["actions"] = actions
	base := strings.TrimRight(s.options.PublicURL, "/")
	if base == "" {
		base = origin
	}
	out["url"] = base + "/#/drafts/" + text(d["id"])
	return out
}

func (s *Service) acceptedSubmissionScheduled(d Record) bool {
	if d["status"] != "auto_ready" || d["_stopped"] == true || d["turnStopped"] == true || d["attempted"] == true || d["issue"] != nil || has(d, "issues") {
		return false
	}
	r := record(d["submissionRequest"])
	rev, ok := integer(r["revision"])
	current, currentOK := integer(d["revision"])
	_, timeErr := time.Parse(time.RFC3339Nano, text(r["at"]))
	owner, ownerOK := r["owner"].(string)
	kind := text(r["kind"])
	if !ok || rev < 0 || !currentOK || current <= rev || timeErr != nil || !ownerOK || !(kind == string(auth.Screen) && owner == "" || kind == string(auth.PC) && strings.HasPrefix(owner, "token:") && strings.TrimSpace(strings.TrimPrefix(owner, "token:")) != "") {
		return false
	}
	s.engineMu.RLock()
	e := s.submissionEngine
	s.engineMu.RUnlock()
	if e == nil || s.options.SubmissionQueued == nil || s.options.Submit == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.options.Submit != nil && !e.receiptStoppingLocked()
}
