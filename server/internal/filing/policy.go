package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

const automaticDisabled = "自動監視からの新規起票は停止されています。"

var investigationMarkers = regexp.MustCompile(`<!-- issue-desk:(investigation|[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}) -->`)

func investigationBody(body string) bool {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	matches := investigationMarkers.FindAllStringSubmatchIndex(body, -1)
	last := -1
	for i, m := range matches {
		if body[m[2]:m[3]] != "investigation" {
			last = i
		}
	}
	if last < 1 {
		return false
	}
	before, current := matches[last-1], matches[last]
	return body[before[2]:before[3]] == "investigation" && before[1]+1 == current[0]
}
func truth(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		return x != "0"
	}
	return true
}
func assertManual(d Record) error {
	if d == nil {
		return fail(409, automaticDisabled)
	}
	status := fleet.Text(d["status"])
	if d["kind"] == "investigation" || d["workType"] == "investigation" || row(d["view"])["workType"] == "investigation" || status == "investigation_waiting" || status == "investigation_reported" || status == "investigation_ended" || investigationBody(fleet.Text(d["issueBody"])) || investigationBody(fleet.Text(d["body"])) {
		return contract.NewProblem(409, "調査Issueは廃止されています。起票・配布・再開・実行はできません。", "investigation_disabled")
	}
	for _, raw := range fleet.Items(d["splitIssues"]) {
		if e := assertManual(row(raw)); e != nil {
			return e
		}
	}
	for _, k := range []string{"proposal", "attention", "usageReviewRound", "priorityReviewRound", "inventoryRound", "stallFix", "fullTestFix", "fullTestRun", "appHealth", "duplicateFalsePositive"} {
		if truth(d[k]) {
			return contract.NewProblem(409, automaticDisabled, "automatic_filing_disabled")
		}
	}
	return nil
}
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	e := db.QueryRowContext(ctx, tableExistsSQL(), name).Scan(&n)
	return n > 0, e
}

// CheckPolicy follows provenance rather than the caller's recovery mode. A manual
// retry cannot convert an automatically generated request into permitted work.
func CheckPolicy(ctx context.Context, records *consultations.Records, draft Record, readIncident ...func(context.Context) (any, error)) error {
	if records == nil || records.DB == nil {
		return fail(503, "起票の来歴を確認できません。")
	}
	db := records.DB
	temporary, e := tableExists(ctx, db, "temporary_code_records")
	if e != nil {
		return e
	}
	value, e := readIncidentLedger(ctx, db, readIncident)
	if e != nil {
		return e
	}
	incidentIDs := incidentFiledIDs(value)
	seen := map[string]bool{}
	current := draft
	for depth := 0; ; depth++ {
		if depth > 64 || current == nil {
			return contract.NewProblem(409, automaticDisabled, "automatic_filing_disabled")
		}
		if e = assertManual(current); e != nil {
			return e
		}
		id := fleet.Text(current["id"])
		if id != "" {
			if seen[id] {
				return contract.NewProblem(409, automaticDisabled, "automatic_filing_disabled")
			}
			seen[id] = true
		}
		if incidentIDs[id] {
			return contract.NewProblem(409, automaticDisabled, "automatic_filing_disabled")
		}
		if temporary && id != "" {
			var n int
			if e = db.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM temporary_code_records WHERE "+jsonText("json", "$.request.id")+"=?"), id).Scan(&n); e != nil {
				return e
			}
			if n > 0 {
				return contract.NewProblem(409, automaticDisabled, "automatic_filing_disabled")
			}
		}
		parent := fleet.Text(current["parentSplit"])
		if parent == "" {
			parent = fleet.Text(current["parentConsultation"])
		}
		if parent == "" {
			break
		}
		var found bool
		current, found, e = records.Get(ctx, "drafts", parent)
		if e != nil {
			return e
		}
		if !found {
			return contract.NewProblem(409, automaticDisabled, "automatic_filing_disabled")
		}
	}
	return unresolvedBasisConflict(draft)
}

// readIncidentLedger reads the incident ledger from the callback, or from the saved settings.
func readIncidentLedger(ctx context.Context, db *sql.DB, readIncident []func(context.Context) (any, error)) (any, error) {
	var e error
	var value any
	if len(readIncident) > 0 && readIncident[0] != nil {
		value, e = readIncident[0](ctx)
		if e != nil {
			return nil, e
		}
	} else {
		settings, err := tableExists(ctx, db, "settings")
		if err != nil {
			return nil, err
		}
		if settings {
			var raw string
			err = db.QueryRowContext(ctx, bindSQL(`SELECT json FROM settings WHERE id='incident-deadline'`)).Scan(&raw)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			if err == nil {
				if err = json.Unmarshal([]byte(raw), &value); err != nil {
					return nil, fmt.Errorf("incident ledger invalid: %w", err)
				}
				if m, ok := value.(map[string]any); ok {
					value = m["value"]
				}
			}
		}
	}
	return value, nil
}

// incidentFiledIDs lists the requests that incident handling filed automatically.
func incidentFiledIDs(value any) map[string]bool {
	incidentIDs := map[string]bool{}
	for _, rawRow := range fleet.Items(value) {
		for _, rawAction := range fleet.Items(row(rawRow)["actions"]) {
			action := row(rawAction)
			if action["action"] == "file" {
				incidentIDs[fleet.Text(action["id"])] = true
			}
		}
	}
	return incidentIDs
}

// unresolvedBasisConflict refuses filing while a decision-basis conflict lacks its resolution.
func unresolvedBasisConflict(draft Record) error {
	for _, raw := range fleet.Items(draft["basisConflicts"]) {
		c := row(raw)
		if fleet.Text(c["fact"]) == "" || fleet.Text(c["reason"]) == "" {
			continue
		}
		// A conflict of any kind is settled once the requester's correction is reflected.
		if strings.TrimSpace(fleet.Text(c["resolution"])) != "" {
			continue
		}
		return contract.NewProblem(409, "依頼の根拠に食い違いがあります。訂正した事実と判断への影響を確認してください。", "DECISION_BASIS_MISMATCH")
	}
	return nil
}
func (s *Service) policy(ctx context.Context, d Record) error {
	// Only the proposal controller can validate this exception. Generic Prepare
	// and CheckPolicy continue to reject all other automatic filing sources.
	if truth(d["proposal"]) && s.options.ValidateFollowup != nil {
		if err := s.options.ValidateFollowup(ctx, d); err != nil {
			return err
		}
		d = clone(d)
		delete(d, "proposal")
	}
	return CheckPolicy(ctx, s.options.Records, d, s.options.IncidentLedger)
}
