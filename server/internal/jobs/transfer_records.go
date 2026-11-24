package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"reflect"
	"regexp"
	"strings"
	"unicode"
)

var (
	titleNumberPattern      = regexp.MustCompile(`#[0-9]+`)
	titleOpenSpacePattern   = regexp.MustCompile(`([（(「\[])\s+`)
	titleCloseSpacePattern  = regexp.MustCompile(`\s+([）)」\]])`)
	titleEmptyParensPattern = regexp.MustCompile(`[（(]\s*[）)]`)
	titleNewlinePattern     = regexp.MustCompile(`\r\n|[\r\n]`)
)

func splitTitle(value string) string {
	v := strings.Join(strings.Fields(value), " ")
	v = titleNumberPattern.ReplaceAllString(v, "")
	v = titleOpenSpacePattern.ReplaceAllString(v, "$1")
	v = titleCloseSpacePattern.ReplaceAllString(v, "$1")
	v = titleEmptyParensPattern.ReplaceAllString(v, "")
	v = strings.Join(strings.Fields(v), " ")
	meaningful := false
	for _, r := range v {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			meaningful = true
		}
	}
	if !meaningful {
		return "依頼の続き"
	}
	return v
}
func proposalID(source, title string) string {
	sum := sha256.Sum256([]byte(source + "\nsplit\n" + title))
	return "split-" + hex.EncodeToString(sum[:])
}
func proposalText(value any, max int) bool {
	s, ok := value.(string)
	return ok && strings.TrimSpace(s) != "" && contract.UTF16Length(s) <= max
}
func transferBindings(job *Job, result map[string]any) (map[string]any, error) {
	value, exists := result["acceptanceTransfers"]
	if !exists {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok || len(items) > 30 {
		return nil, contract.NewProblem(400, "acceptanceTransfersは30件以内の配列です。", "")
	}
	proposals := []any{}
	if raw, exists := result["followupIssues"]; exists {
		var ok bool
		proposals, ok = raw.([]any)
		if !ok {
			return nil, contract.NewProblem(400, "followupIssuesは配列です。", "")
		}
	}
	if len(proposals) > 25 {
		return nil, contract.NewProblem(400, "分割案は25件以内です。", "")
	}
	for _, raw := range proposals {
		p := fleet.Object(raw)
		acceptance, ok := p["acceptance"].([]any)
		if p == nil || !proposalText(p["title"], 200) || !proposalText(p["purpose"], 8000) || !ok || len(acceptance) == 0 || len(acceptance) > 30 || p["kind"] != nil && p["kind"] != "implementation" || p["workType"] == "investigation" {
			return nil, contract.NewProblem(400, "分割案のタイトル・目的・受入条件が不正です。", "")
		}
		for _, text := range acceptance {
			if !proposalText(text, 2000) {
				return nil, contract.NewProblem(400, "分割案の受入条件が不正です。", "")
			}
		}
		if project, exists := p["project"]; exists {
			if _, ok := project.(string); !ok || project != "" && !fleet.ValidRepo(fleet.Text(project)) {
				return nil, contract.NewProblem(400, "分割案の対象リポジトリが不正です。", "")
			}
		}
	}
	transfers := []any{}
	numbers := []int64{}
	seen := map[int64]bool{}
	for _, raw := range items {
		v := fleet.Object(raw)
		artifacts, aOK := v["artifacts"].([]any)
		followups, fOK := v["followupNumbers"].([]any)
		if v == nil || !knownKeys(v, "criterion", "artifacts", "followupNumbers") || !proposalText(v["criterion"], 2000) || !aOK || len(artifacts) == 0 || len(artifacts) > 30 || !fOK || len(followups) == 0 {
			return nil, contract.NewProblem(400, "移管する受入条件・成果物・案番号が不正です。", "")
		}
		clean := []any{}
		for _, a := range artifacts {
			if !proposalText(a, 2000) {
				return nil, contract.NewProblem(400, "成果物の形が違います。", "")
			}
			clean = append(clean, strings.TrimSpace(fleet.Text(a)))
		}
		unique := map[int64]bool{}
		for _, raw := range followups {
			n, ok := count(raw)
			if !ok || n < 1 || n > int64(len(proposals)) || unique[n] {
				return nil, contract.NewProblem(400, "移管先の案番号が不正です。", "")
			}
			unique[n] = true
			if !seen[n] {
				seen[n] = true
				numbers = append(numbers, n)
			}
		}
		transfers = append(transfers, map[string]any{"criterion": strings.TrimSpace(fleet.Text(v["criterion"])), "artifacts": clean, "followupNumbers": followups})
	}
	if len(items) == 0 {
		proposals = []any{}
	}
	report := map[string]any{"acceptanceTransfers": transfers, "followupIssues": proposals}
	text, _ := usage.Compact([]byte(transferJSON(report)))
	if contract.UTF16Length(text) > 24*1024 {
		return nil, contract.NewProblem(400, "移管する受入条件と案の報告が大きすぎます。", "")
	}
	source := transferKey(job.Repo, job.Issue)
	bindings := []any{}
	for _, number := range numbers {
		p := fleet.Object(proposals[number-1])
		title := fleet.Text(p["title"])
		legacy := titleNewlinePattern.ReplaceAllString(title, " ")
		bindings = append(bindings, map[string]any{"number": number, "id": proposalID(source, splitTitle(title)), "legacyId": proposalID(source, legacy), "status": "missing", "reason": "対応する受付の案記録がありません", "evidence": []any{}})
	}
	provider := "codex"
	if job.Provider != nil {
		provider = *job.Provider
	}
	return map[string]any{"version": 1, "provider": provider, "source": source, "report": report, "proposals": bindings}, nil
}
func knownKeys(v map[string]any, keys ...string) bool {
	for key := range v {
		found := false
		for _, k := range keys {
			if k == key {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// comparableTransferReport revalidates old records before applying the same
// normalization used for new reports. Invalid or incomplete records must not
// acquire a meaning by inference.
func comparableTransferReport(job *Job, report map[string]any) (any, bool) {
	bindings, err := transferBindings(job, report)
	if err != nil || bindings == nil {
		return nil, false
	}
	return bindings["report"], true
}

func sameTransferReport(job *Job, previous, next map[string]any) bool {
	a, aOK := comparableTransferReport(job, previous)
	b, bOK := comparableTransferReport(job, next)
	return aOK && bOK && equivalent(a, b)
}

func checkStoredTransferReport(job *Job, result map[string]any) error {
	next, err := transferBindings(job, result)
	if err != nil || next == nil {
		return err
	}
	previous := fleet.Object(job.View["acceptanceTransferRecords"])
	if previous == nil {
		return nil
	}
	oldReport := fleet.Object(previous["report"])
	if !knownKeys(oldReport, "acceptanceTransfers", "followupIssues") || !sameTransferReport(job, oldReport, fleet.Object(next["report"])) {
		return contract.NewProblem(409, "保存済みのacceptanceTransfersと報告が一致しません。", "acceptance_transfer_conflict")
	}
	return nil
}

// sameTransferReplay keeps every other result field subject to the ordinary
// replay comparison, and never rewrites the saved result or transfer record.
func sameTransferReplay(job *Job, report Report, view map[string]any) bool {
	if job.Result == nil || report.Result == nil || len(fleet.List(job.Result["acceptanceTransfers"])) == 0 || !sameTransferReport(job, job.Result, report.Result) {
		return false
	}
	copyJob := *job
	copyJob.Result = make(map[string]any, len(job.Result))
	for k, v := range job.Result {
		copyJob.Result[k] = v
	}
	copyReport := report
	copyReport.Result = make(map[string]any, len(report.Result))
	for k, v := range report.Result {
		copyReport.Result[k] = v
	}
	for _, key := range []string{"acceptanceTransfers", "followupIssues"} {
		delete(copyJob.Result, key)
		delete(copyReport.Result, key)
	}
	return sameReport(&copyJob, copyReport, view)
}
func (s *Store) reportTransfers(ctx context.Context, current *Job, result map[string]any) (map[string]any, error) {
	next, err := transferBindings(current, result)
	if err != nil || next == nil {
		return nil, err
	}
	previous := fleet.Object(current.View["acceptanceTransferRecords"])
	if previous != nil {
		if err := checkStoredTransferReport(current, result); err != nil {
			return nil, err
		}
		next = previous
	}
	return s.resolveTransferRecords(ctx, next)
}
func (s *Store) readTransferLocal(ctx context.Context, table, id string) (map[string]any, error) {
	if s.options.AppDB == nil {
		return nil, nil
	}
	var query string
	switch table {
	case "drafts":
		query = bindSQL("SELECT json FROM drafts WHERE id=?")
	case "proposals":
		query = bindSQL("SELECT json FROM proposals WHERE id=?")
	default:
		return nil, fmt.Errorf("unknown transfer record table")
	}
	var raw string
	err := s.options.AppDB.QueryRowContext(ctx, query, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var row map[string]any
	err = json.Unmarshal([]byte(raw), &row)
	return row, err
}
func (s *Store) resolveTransferRecords(ctx context.Context, saved map[string]any) (map[string]any, error) {
	if s.options.AppDB == nil {
		return saved, nil
	}
	source := fleet.Text(saved["source"])
	bindings := fleet.List(saved["proposals"])
	numbers := fleet.List(fleet.Object(saved["report"])["followupIssues"])
	out := fleet.Object(fleet.Clone(saved))
	resolved := []any{}
	for _, raw := range bindings {
		b := fleet.Object(raw)
		number := int64(fleet.Number(b["number"]))
		if number < 1 || number > int64(len(numbers)) {
			return nil, fmt.Errorf("invalid stored transfer proposal")
		}
		expected := fleet.Object(numbers[number-1])
		result, e := s.resolveTransferProposal(ctx, saved, source, b, expected, number)
		if e != nil {
			return nil, e
		}
		resolved = append(resolved, result)
	}
	out["proposals"] = resolved
	return out, nil
}

// resolveTransferProposal は移管の案1件の状態を、受付の案・破棄・拒否の記録と保存済みの下書きから決める。
func (s *Store) resolveTransferProposal(ctx context.Context, saved map[string]any, source string, b, expected map[string]any, number int64) (map[string]any, error) {
	result := map[string]any{"number": number, "id": b["id"], "legacyId": b["legacyId"], "status": "missing", "reason": "対応する受付の案記録がありません", "evidence": []any{}}
	if b["issue"] != nil {
		result["issue"] = b["issue"]
	}
	observations, e := s.readTransferObservations(ctx, source, []string{fleet.Text(b["id"]), fleet.Text(b["legacyId"])})
	if e != nil {
		return nil, e
	}
	for _, observed := range observations {
		if observed["type"] == "proposal" && observed["issue"] != nil && result["issue"] == nil {
			result["issue"] = observed["issue"]
		}
	}
	var record map[string]any
	for _, r := range observations {
		result["evidence"] = append(fleet.List(result["evidence"]), map[string]any{"type": r["type"], "id": r["id"], "status": r["status"], "commentId": r["commentId"], "fingerprint": r["fingerprint"]})
		if r["type"] == "proposal" {
			if record != nil {
				result["status"], result["reason"] = "conflict", "新旧IDの案記録が競合しています"
				record = nil
				break
			}
			record = r
		}
	}
	if record == nil && result["status"] != "conflict" {
		fingerprints := transferFingerprints(saved, b, expected)
		for _, observed := range observations {
			matched := false
			for _, fingerprint := range fingerprints {
				if observed["fingerprint"] == fingerprint {
					matched = true
				}
			}
			if !matched {
				continue
			}
			if observed["type"] == "dismissal" && observed["status"] == "dismissed" {
				result["status"], result["reason"] = "discarded", "同じ内容の案が破棄されています"
				break
			}
			if observed["type"] == "rejection" {
				result["status"], result["reason"] = "rejected", "同じ内容の案の拒否記録があります"
			}
		}
	}
	if record != nil {
		if e := s.resolveRecordedTransferProposal(ctx, result, record, source, expected); e != nil {
			return nil, e
		}
	}
	return result, nil
}

// resolveRecordedTransferProposal は対応する案の記録がある案の状態を、案の内容・下書き・起票先の一致で決める。
func (s *Store) resolveRecordedTransferProposal(ctx context.Context, result, record map[string]any, source string, expected map[string]any) error {
	if issue := record["issue"]; issue != nil {
		if result["issue"] != nil && !reflect.DeepEqual(result["issue"], issue) {
			result["status"], result["reason"] = "conflict", "既知の起票先と現在の記録が一致しません"
			return nil
		}
		result["issue"] = issue
	}
	proposal := fleet.Object(record["proposal"])
	if !matchesTransferProposal(proposal, source, expected) || proposal["id"] != record["id"] || len(fleet.List(record["conflicts"])) > 0 {
		result["status"], result["reason"] = "conflict", "同じ案IDの内容が一致しません"
	} else {
		draft, e := s.readTransferLocal(ctx, "drafts", fleet.Text(record["draftId"]))
		if e != nil {
			return e
		}
		result["proposalId"] = record["id"]
		switch {
		case record["status"] == "discarded" || draft == nil && record["issue"] == nil:
			result["status"], result["reason"] = "discarded", "案の下書きが破棄されたか起票せず終了しました"
		case draft != nil && !matchesTransferDraft(draft, record, expected):
			result["status"], result["reason"] = "conflict", "保存済み下書きと案の内容が一致しません"
		default:
			issue := fleet.Object(record["issue"])
			if issue == nil && draft["status"] == "submitted" {
				issue = fleet.Object(draft["issue"])
			}
			if issue != nil {
				n, valid := count(issue["number"])
				if issue["project"] != record["repo"] || !valid || n < 1 {
					result["status"], result["reason"] = "unknown", "起票先のリポジトリまたはIssue番号が不明です"
				} else if draft["issue"] != nil && record["issue"] != nil && !reflect.DeepEqual(draft["issue"], record["issue"]) {
					result["status"], result["reason"] = "conflict", "下書きと案の起票先が一致しません"
				} else {
					result["issue"] = issue
					result["status"], result["reason"] = "filed", "保存済み起票先を確認しました"
				}
			} else {
				result["status"], result["reason"] = "unfiled", "対応案の起票先を確認できません"
			}
		}
	}
	return nil
}

// Empty proposal arrays are the only records whose output cannot change when
// observations or drafts change. Missing or malformed arrays remain eligible.
func (s *Store) transferReconciliationCandidates(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE view?'acceptanceTransferRecords'
 AND NOT (status = 'merged' AND COALESCE(view->'acceptanceTransferRecords'->'proposals' = '[]'::jsonb, false)) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	return ids, err
}

func (s *Store) ReconcileTransferRecords(ctx context.Context) error {
	ids, err := s.transferReconciliationCandidates(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.transaction(ctx, func(tx *sql.Tx) error {
			job, e := readJob(ctx, tx, id, true)
			if e != nil {
				return e
			}
			saved := fleet.Object(job.View["acceptanceTransferRecords"])
			if saved == nil {
				return nil
			}
			next, e := s.resolveTransferRecords(ctx, saved)
			if e != nil {
				return e
			}
			if reflect.DeepEqual(saved, next) {
				return nil
			}
			_, e = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferRecords}',$2::jsonb)WHERE id=$1", id, transferJSON(next))
			return e
		}); err != nil {
			return err
		}
	}
	return nil
}
