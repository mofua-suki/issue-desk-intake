package filingcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"strings"
)

func fingerprint(material Record) string {
	sum := sha256.Sum256(raw(material))
	return hex.EncodeToString(sum[:])
}
func (s *Service) Check(ctx context.Context, original Record, triggers ...string) error {
	lock := s.lock(fleet.Text(original["id"]))
	lock.Lock()
	defer lock.Unlock()
	d := clone(original)
	if err := s.checkSplitDependencies(ctx, d); err != nil {
		return err
	}
	trigger := "automatic_recovery"
	if len(triggers) > 0 && triggers[0] != "" {
		trigger = triggers[0]
	}
	material, err := s.material(ctx, d)
	if err != nil {
		return err
	}
	material = clone(material)
	snapshot := fingerprint(material)
	if hold := obj(d["filingCheckHold"]); hold != nil && trigger == "automatic_recovery" && hold["fingerprint"] == snapshot {
		return fail("起票前の確認を停止中です: " + fleet.Text(hold["reason"]) + "。必要な資料を相談へ追加し再送してください。")
	}
	for _, item := range fleet.Elements(material["explicitDependencies"]) {
		dep := obj(item)
		if strings.TrimSpace(fleet.Text(dep["reason"])) != "" {
			return fail("依存の状態が未確認です: " + fleet.Text(dep["reason"]))
		}
	}
	if !hasFilingComparison(material) {
		calls := obj(d["filingCheckCalls"])
		if calls != nil {
			delete(calls, "evidenceHold")
		}
		return s.update(ctx, d, Record{"filingCheckHold": nil, "filingCheckCalls": calls})
	}
	calls := filingCallsForRevision(d)
	if calls["pending"] == true && calls["fingerprint"] == snapshot {
		return fail("前回の起票前判定の結果が確認できません。相談を修正して明示的に再送してください。")
	}
	if resetFilingBudgetForRepair(calls) {
		delete(calls, "evidenceHold")
		calls["count"] = 0
		calls["results"] = []any{}
		calls["repairRecheckPending"] = true
	}
	budget := loadFilingBudget(calls, d["revision"])
	if budget.Pending || budget.Decisions >= filingDecisionLimit {
		return fail("読み足しは上限8回、起票前の判定は別枠で上限5回です。判定の上限に達したか、前回の結果が確認できません。相談を更新して再送してください。")
	}
	if hold := obj(calls["evidenceHold"]); trigger == "automatic_recovery" && hold != nil && hold["fingerprint"] == snapshot {
		reason := fleet.Text(hold["reason"])
		if reason == "" {
			reason = "資料不足: 必要な根拠を確認できません。必要な相談を consultation:<相談ID> で指定し、必要に応じ節・行範囲を指定して再送してください。"
		}
		return fail(reason)
	}
	prior := obj(d["filingCheckHold"])
	if prior != nil {
		material["previousRefusal"] = prior
		material["alternativeIssues"] = alternativeIssues(material)
	}
	current := material
	var evidenceRequests, allRequests []any
	attempted := map[string]bool{}
	var previousMissing, selectionMissing []any
	var lastSent Record
	// 求めた資料が容量上限だけで収まらないときは、追加取得なしの最終判定を1回行う。
	forcedFinal := false
	var forcedDetail string
	forcedIDs := map[string]bool{}
	for phase := 0; ; phase++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget.Decisions >= filingDecisionLimit || budget.Pending {
			return fail("読み足しは上限8回、起票前の判定は別枠で上限5回です。判定の上限に達しました。相談を更新して再送してください。")
		}
		finalDecision := budget.finalDecision() || forcedFinal
		if finalDecision {
			current["evidenceBudgetFinal"] = true
		}
		current = s.fitInput(current, nil)
		if size := s.inputSize(current); size > inputLimit {
			return fail("起票前の資料が入力上限を超えています。" + sizeBreakdown(current, size, len(s.schema)) + "対象本文・会話・必要な候補を節や行範囲に絞って再送してください。")
		}
		if err := budget.reserve(); err != nil {
			return err
		}
		check, err := s.recordCheckStart(ctx, d, calls, &budget, current, snapshot, trigger, phase)
		if err != nil {
			return err
		}
		lastSent = clone(current)
		answer, callErr := s.askFilingCheck(ctx, d, current, finalDecision)
		if callErr != nil {
			if err := s.recordAIFailure(ctx, d, calls, check, &budget, callErr); err != nil {
				return err
			}
			return callErr
		}
		// Capture the AI's declaration before duplicateVerdict can override it.
		reportedCategory := fleet.Text(answer["refusalCategory"])
		decision := duplicateVerdict(Record(answer), current)
		requested := fleet.Elements(decision["evidenceRequests"])
		result := "decision"
		if len(requested) > 0 && !forcedFinal {
			result = "evidence_requested"
		}
		// 容量上限による最終判定での再要求は、読み足しではなく判定1回として数える。
		_ = budget.finish(result)
		budget.save(calls)
		if finalDecision && len(requested) > 0 {
			reason := finalEvidenceReason(requested, forcedIDs, forcedFinal, forcedDetail)
			check["result"], check["reason"] = "refused", safeCheckReason("evidence_requested", reason, current)
			if err := s.saveEvidenceHold(ctx, d, calls, Record{"fingerprint": snapshot, "requests": append(allRequests, requested...), "evidenceMissing": uniqueEvidenceMissing(append(previousMissing, selectionMissing...)), "reason": reason, "at": stamp(s.options.Now())}); err != nil {
				return err
			}
			return fail(reason)
		}
		deps, err := dependencies(decision, current)
		if err != nil {
			if saveErr := s.recordInvalidAnswer(ctx, d, calls, check, err, current); saveErr != nil {
				return saveErr
			}
			return err
		}
		if err := s.numberSystem(ctx, d, deps, current); err != nil {
			return err
		}
		if len(requested) > 0 {
			allRequests = append(allRequests, requested...)
			fresh := freshEvidenceRequests(requested, attempted)
			check["result"] = "evidence_requested"
			check["reason"] = evidenceCheckReason(requested)
			calls["pending"], calls["outcome"] = false, "evidence_requested"
			if err := s.update(ctx, d, Record{"filingCheckCalls": calls}); err != nil {
				return err
			}
			var breakdown string
			var missing []any
			if len(fresh) == 0 {
				missing, breakdown = s.repeatedRequestMissing(previousMissing, selectionMissing, current)
			}
			if len(fresh) > 0 {
				expanded, err := s.expandEvidence(ctx, d, material, append(evidenceRequests, fresh...), fresh, selectionMissing)
				if err != nil {
					return err
				}
				current, evidenceRequests, missing, breakdown = expanded.current, expanded.requests, expanded.missing, expanded.breakdown
				previousMissing = expanded.previousMissing
				selectionMissing = nil
			}
			missing = uniqueEvidenceMissing(missing)
			recoverable := evidenceRecoverable(missing)
			if len(fresh) > 0 && recoverable {
				selectionMissing = missing
				current["evidenceMissing"] = missing
			}
			// 自動重複照合は、相談の省略例外または起票済み本文不足の
			// 拒否分類を次の判定で確定する必要がある。
			duplicateOmitted := duplicateBodyOmitted(decision, fresh, missing, previousMissing, current)
			noProgress := len(fresh) > 0 && !recoverable && !duplicateOmitted && !hasNewEvidence(lastSent, current)
			if len(fresh) == 0 || noProgress || len(missing) > 0 && !recoverable {
				missing = uniqueEvidenceMissing(append(missing, previousMissing...))
				if !finalDecision && inputLimitOnlyMissing(missing) {
					forcedFinal = true
					for _, item := range missing {
						forcedIDs[fleet.Text(obj(item)["candidateId"])] = true
					}
					forcedDetail = missingBudgetDetails(missing) + "。" + sizeBreakdown(current, s.inputSize(current), len(s.schema))
					if id := fleet.Text(decision["duplicateBodyRequired"]); id != "" {
						current["duplicateBodyRequired"] = id
					}
					continue
				}
				if detail := missingBudgetDetails(missing); detail != "" {
					breakdown += detail + "。"
				}
				reason := "資料不足: 追加取得後も必要な根拠を確認できません。" + breakdown + "必要な相談を consultation:<相談ID> で指定し、必要に応じ節・行範囲を指定して再送してください。"
				check["reason"] = safeCheckReason("evidence_requested", reason, current)
				if err := s.saveEvidenceHold(ctx, d, calls, Record{"fingerprint": snapshot, "requests": allRequests, "evidenceMissing": missing, "reason": reason, "at": stamp(s.options.Now())}); err != nil {
					return err
				}
				return fail(reason)
			}
			if id := fleet.Text(decision["duplicateBodyRequired"]); id != "" {
				current["duplicateBodyRequired"] = id
			}
			continue
		}
		if len(selectionMissing) > 0 {
			if err := s.saveEvidenceHold(ctx, d, calls, Record{"fingerprint": snapshot, "requests": allRequests, "evidenceMissing": uniqueEvidenceMissing(selectionMissing), "reason": "資料不足: 選択失敗が未解消です。必要な相談を consultation:<相談ID> で指定し、節または行範囲を指定してください。", "at": stamp(s.options.Now())}); err != nil {
				return err
			}
			return fail(fleet.Text(obj(calls["evidenceHold"])["reason"]))
		}
		return s.saveVerdict(ctx, d, original, calls, check, decision, deps, current, snapshot, prior, reportedCategory)
	}
}

// evidenceExpansion is the material of the next decision after reading the requested evidence.
type evidenceExpansion struct {
	current Record
	// requests keeps the requests to read again in the next expansion, without the failed ones.
	requests []any
	// missing lists the evidence the next decision still lacks.
	missing         []any
	previousMissing []any
	breakdown       string
}

// expandEvidence reads the requested evidence into the material, fits it into the input
// limit, and lists the evidence that is still missing.
func (s *Service) expandEvidence(ctx context.Context, d, material Record, requests, fresh, selectionMissing []any) (evidenceExpansion, error) {
	current, err := s.expand(ctx, d, material, requests)
	if err != nil {
		return evidenceExpansion{}, err
	}
	if recentOmitted(current, fresh) {
		current, err = s.expand(ctx, d, material, prioritizeRecent(requests, fresh))
		if err != nil {
			return evidenceExpansion{}, err
		}
	}
	missing := append([]any(nil), fleet.Elements(current["evidenceMissing"])...)
	// Never accumulate a failed request into the next expansion.
	requests = withoutFailedEvidence(requests, current)
	missing = append(missing, outsideCandidateMissing(current)...)
	// 入力上限への縮小で落とした読み足しも省略として残すため、縮小の後に数える。
	current = s.fitInput(current, fresh)
	// Resolve failures only with material actually retained for the next call.
	missing = append(missing, unresolvedSelections(selectionMissing, fresh, current)...)
	previousMissing, _ := omittedMissing(current)
	missing = append(missing, countLimitMissing(current)...)
	limitMissing, breakdown := s.inputLimitMissing(current, fresh)
	missing = append(missing, limitMissing...)
	return evidenceExpansion{current: current, requests: requests, missing: missing, previousMissing: previousMissing, breakdown: breakdown}, nil
}

// filingCallsForRevision returns the saved call record of this revision, or starts a new one
// that keeps the check history of the earlier revision.
func filingCallsForRevision(d Record) Record {
	calls := obj(d["filingCheckCalls"])
	if calls == nil || fleet.WholeNumber(calls["revision"]) != fleet.WholeNumber(d["revision"]) {
		history := fleet.Elements(calls["checks"])
		for _, item := range history {
			if entry := obj(item); entry != nil && entry["revision"] == nil {
				entry["revision"] = calls["revision"]
			}
		}
		calls = Record{"revision": d["revision"], "count": 0, "results": []any{}, "checks": history, "usedCodeRevisions": []any{}}
	}
	return calls
}

// finalEvidenceReason explains which materials the final decision still asked for.
// After a final decision forced by the input limit, it separates materials dropped by
// the limit from requests for other materials.
func finalEvidenceReason(requested []any, forcedIDs map[string]bool, forcedFinal bool, forcedDetail string) string {
	parts := make([]string, 0, len(requested))
	var omittedParts, otherParts []string
	for _, item := range requested {
		r := obj(item)
		selection := "全範囲"
		if r["selection"] != nil {
			selection = string(raw(r["selection"]))
		}
		part := fmt.Sprintf("候補ID=%s 資料種別=%s 必要範囲=%s", limitCheckReason(contract.MaskCredentials(fleet.Text(r["candidateId"]))), limitCheckReason(contract.MaskCredentials(fleet.Text(r["kind"]))), limitCheckReason(contract.MaskCredentials(selection)))
		parts = append(parts, part)
		if forcedIDs[fleet.Text(r["candidateId"])] {
			omittedParts = append(omittedParts, part)
		} else {
			otherParts = append(otherParts, part)
		}
	}
	reason := "資料不足: 読み足し上限後の判定に必要な資料: " + strings.Join(parts, "; ") + "。必要な資料を追加して再送してください。"
	if forcedFinal {
		// 容量上限で落ちた資料の再要求と、別の候補の要求を分けて書く。
		reason = "資料不足: "
		if len(omittedParts) > 0 {
			reason += "容量上限で読めない資料が判定に不可欠です: " + strings.Join(omittedParts, "; ") + "。"
		}
		if len(otherParts) > 0 {
			reason += "容量上限で一部の資料を読めないままの最終判定で、別の資料が求められました: " + strings.Join(otherParts, "; ") + "。"
		}
		reason += forcedDetail
		if len(omittedParts) > 0 {
			reason += "必要な相談を consultation:<相談ID> で指定し、節・行範囲に絞って再送してください。"
		}
		if len(otherParts) > 0 {
			reason += "別の資料は必要な箇所を相談の本文へ書き足して再送してください。"
		}
	}
	return reason
}

// freshEvidenceRequests keeps the requests not attempted before and marks them attempted.
func freshEvidenceRequests(requested []any, attempted map[string]bool) []any {
	fresh := []any{}
	for _, item := range requested {
		r := obj(item)
		key := fleet.Text(r["candidateId"]) + "\x00" + fleet.Text(r["kind"]) + "\x00" + string(raw(r["selection"]))
		if !attempted[key] {
			fresh = append(fresh, item)
			attempted[key] = true
		}
	}
	return fresh
}

// withoutFailedEvidence drops the requests whose material could not be obtained.
func withoutFailedEvidence(evidenceRequests []any, current Record) []any {
	kept := []any{}
	for _, req := range evidenceRequests {
		r := obj(req)
		failed := false
		for _, m := range fleet.Elements(current["evidenceMissing"]) {
			x := obj(m)
			if x["candidateId"] == r["candidateId"] && x["kind"] == r["kind"] && string(raw(x["selection"])) == string(raw(r["selection"])) {
				failed = true
			}
		}
		if !failed {
			kept = append(kept, req)
		}
	}
	return kept
}

// evidenceRecoverable reports whether every missing material is a selection the next call can correct.
func evidenceRecoverable(missing []any) bool {
	recoverable := len(missing) > 0
	for _, item := range missing {
		if reason := fleet.Text(obj(item)["reason"]); !strings.HasPrefix(reason, "選択失敗:") && !strings.HasPrefix(reason, "目的・受入の節がありません") {
			recoverable = false
		}
	}
	return recoverable
}

// duplicateBodyOmitted reports whether the only fresh request is the duplicate body that the
// input limit dropped, so the next decision must settle the duplicate refusal.
func duplicateBodyOmitted(decision Record, fresh, missing, previousMissing []any, current Record) bool {
	duplicateID := fleet.Text(decision["duplicateBodyRequired"])
	duplicateOmitted := false
	if duplicateID != "" && len(fresh) == 1 && obj(fresh[0])["candidateId"] == duplicateID && len(missing) == 0 {
		for _, item := range previousMissing {
			if obj(item)["candidateId"] == duplicateID && obj(item)["reason"] == omissionInputLimitReason {
				duplicateOmitted = omittedDuplicateBody(current, duplicateID) || strings.HasPrefix(duplicateID, "request:") || strings.HasPrefix(duplicateID, "job:")
			}
		}
	}
	return duplicateOmitted
}

// saveVerdict records the final allow or refuse decision of the filing check.
func (s *Service) saveVerdict(ctx context.Context, d, original, calls, check, decision Record, deps []any, current Record, snapshot string, prior Record, reportedCategory string) error {
	proceed, valid := decision["proceed"].(bool)
	if !valid || strings.TrimSpace(fleet.Text(decision["reason"])) == "" {
		calls["pending"], calls["outcome"] = false, "invalid_answer"
		check["result"] = "invalid_answer"
		check["reason"] = checkReasonFallback["invalid_answer"]
		if err := s.update(ctx, d, Record{"filingCheckCalls": calls}); err != nil {
			return err
		}
		return fail("起票前の判定形式が不正です。相談で確認してください。")
	}
	calls["pending"] = false
	if !proceed {
		reason := fleet.Text(decision["reason"])
		calls["outcome"], calls["lastRefusal"] = "refused", reason
		check["result"] = "refused"
		check["reason"] = safeCheckReason("refused", reason, current)
		check["refusalCategory"] = refusalCategory(decision, fleet.Elements(check["inputOmissions"]))
		switch reportedCategory {
		case "none", "input_limit_only", "other", "unknown":
			check["reportedRefusalCategory"] = reportedCategory
		}
		hold := Record{"fingerprint": snapshot, "reason": reason, "at": stamp(s.options.Now()), "revision": d["revision"], "alternatives": alternativeIssues(current)}
		if err := s.update(ctx, d, Record{"filingCheckCalls": calls, "filingCheckHold": hold}); err != nil {
			return err
		}
		return fail("起票前の確認で止めました: " + reason + "\n必要な相談を consultation:<相談ID> で指定し、元の目的・担当範囲を保って再送してください。")
	}
	delete(calls, "evidenceHold")
	calls["outcome"], check["result"] = "allowed", "allowed"
	check["reason"] = safeCheckReason("allowed", fleet.Text(decision["reason"]), current)
	calls["results"] = append(fleet.Elements(calls["results"]), "allowed")
	fields := Record{"filingCheckCalls": calls, "filingCheckHold": nil, "dependsOnIssues": combineDependencies(fleet.Elements(d["dependsOnIssues"]), deps)}
	if prior != nil {
		fields["filingRefusalOverturned"] = Record{"at": stamp(s.options.Now()), "previous": prior, "reason": decision["reason"]}
	}
	if err := s.update(ctx, d, fields); err != nil {
		return err
	}
	for key, value := range fields {
		original[key] = value
	}
	return nil
}

// recordCheckStart saves the pending call and its check history entry before the AI call,
// so an interrupted call is not repeated for the same material.
func (s *Service) recordCheckStart(ctx context.Context, d, calls Record, budget *filingBudget, current Record, snapshot, trigger string, phase int) (Record, error) {
	budget.save(calls)
	calls["count"] = fleet.WholeNumber(calls["count"]) + 1
	calls["pending"], calls["fingerprint"], calls["lastTrigger"], calls["codeRevision"] = true, snapshot, trigger, s.options.Revision
	check := Record{"at": stamp(s.options.Now()), "codeRevision": s.options.Revision, "trigger": trigger, "comparisonId": snapshot, "phase": phase, "revision": d["revision"]}
	check["inputOmissions"] = inputOmissions(current)
	calls["checks"] = boundedCheckHistory(append(fleet.Elements(calls["checks"]), check))
	if err := s.update(ctx, d, Record{"filingCheckCalls": calls}); err != nil {
		return nil, err
	}
	return check, nil
}

// askFilingCheck asks the intake AI for the filing decision on the current material.
func (s *Service) askFilingCheck(ctx context.Context, d, current Record, finalDecision bool) (map[string]any, error) {
	callInstructions := instructions
	if finalDecision {
		callInstructions += finalBudgetInstructions
	}
	return s.options.Answer(ctx, map[string]any{"schema": s.schema, "step": usage.FilingCheckStep, "provider": d["consultationProvider"], "implementationProvider": d["provider"], "tier": d["tier"], "project": d["project"], "input": callInstructions + materialSeparator + string(raw(current))})
}

// recordAIFailure saves the failed call and its diagnosis in the check history.
func (s *Service) recordAIFailure(ctx context.Context, d, calls, check Record, budget *filingBudget, callErr error) error {
	_ = budget.finish("ai_failed")
	budget.save(calls)
	calls["pending"], calls["outcome"] = false, "ai_failed"
	check["result"] = "ai_failed"
	failure := answerFailure(d, callErr)
	if failure != nil {
		check["answerFailure"] = failure
	}
	check["reason"] = aiFailedCheckReason(failure)
	return s.update(ctx, d, Record{"filingCheckCalls": calls})
}

// repeatedRequestMissing explains why requests that were all attempted before cannot progress.
func (s *Service) repeatedRequestMissing(previousMissing, selectionMissing []any, current Record) ([]any, string) {
	var breakdown string
	var missing []any
	missing = append(missing, previousMissing...)
	missing = append(missing, selectionMissing...)
	for _, item := range previousMissing {
		if obj(item)["reason"] == omissionInputLimitReason {
			breakdown = sizeBreakdown(current, s.inputSize(current), len(s.schema))
			break
		}
	}
	if len(missing) == 0 {
		missing = append(missing, Record{"reason": "同一の資料選択が繰り返されました", "next": "取得済みの資料を確認するか、別の選択を指定してください。"})
	}
	return missing, breakdown
}

// outsideCandidateMissing reports the requests that named no comparison candidate.
func outsideCandidateMissing(current Record) []any {
	var missing []any
	for _, item := range fleet.Elements(current["evidenceOutsideCandidates"]) {
		r := obj(item)
		missing = append(missing, Record{"candidateId": r["candidateId"], "kind": r["kind"], "selection": r["selection"], "reason": "候補を取得できません"})
	}
	return missing
}

// countLimitMissing reports the requested materials dropped by the candidate count limit.
func countLimitMissing(current Record) []any {
	var missing []any
	for _, item := range fleet.Elements(current["evidenceOmitted"]) {
		r := obj(item)
		if r["reason"] == "件数上限" {
			missing = append(missing, Record{"candidateId": r["candidateId"], "kind": r["kind"], "selection": r["selection"], "reason": r["reason"]})
		}
	}
	return missing
}

// inputLimitMissing reports the fresh materials that do not fit the input limit and extends
// the size breakdown with their detail.
func (s *Service) inputLimitMissing(current Record, fresh []any) ([]any, string) {
	var missing []any
	breakdown := ""
	if size := s.inputSize(current); size > inputLimit {
		unfit, detail := unfitEvidence(current, fresh)
		missing = append(missing, unfit...)
		if detail != "" {
			breakdown += detail + "。"
		}
		breakdown += sizeBreakdown(current, size, len(s.schema))
		missing = append(missing, Record{"reason": "入力上限", "detail": breakdown})
	}
	return missing, breakdown
}

// saveEvidenceHold stops the check for missing materials and saves the hold that blocks an
// automatic retry with the same material.
func (s *Service) saveEvidenceHold(ctx context.Context, d, calls, hold Record) error {
	calls["pending"], calls["outcome"], calls["stopKind"] = false, "evidence_requested", "evidence_missing"
	calls["evidenceHold"] = hold
	return s.update(ctx, d, Record{"filingCheckCalls": calls})
}

// recordInvalidAnswer saves an answer whose dependencies cannot be used.
func (s *Service) recordInvalidAnswer(ctx context.Context, d, calls, check Record, err error, current Record) error {
	calls["pending"], calls["outcome"] = false, "invalid_answer"
	check["result"] = "invalid_answer"
	check["reason"] = safeCheckReason("invalid_answer", err.Error(), current)
	return s.update(ctx, d, Record{"filingCheckCalls": calls})
}

const materialSeparator = "\n確認資料:\n"

// inputLimitOmittedKey は入力上限へ収めるために落とした要約の詳細・候補の記録。
const inputLimitOmittedKey = "inputLimitOmitted"

// inputLimit は受付AIへ送る入力の上限（bytes）。指示・スキーマ・確認資料の合計で数える。
const inputLimit = 160000

func combineDependencies(original, added []any) []any {
	out := append([]any{}, original...)
	seen := map[string]bool{}
	for _, item := range out {
		d := obj(item)
		seen[fmt.Sprint(d["source"], ":", fleet.WholeNumber(d["number"]))] = true
	}
	for _, item := range added {
		d := obj(item)
		key := fmt.Sprint(d["source"], ":", fleet.WholeNumber(d["number"]))
		if !seen[key] {
			out = append(out, item)
			seen[key] = true
		}
	}
	return out
}
func alternativeIssues(material Record) []any {
	out := []any{}
	for _, item := range fleet.Elements(material["requests"]) {
		r := obj(item)
		out = append(out, Record{"number": r["number"], "title": r["title"], "acceptsInstructions": r["acceptsInstructions"]})
	}
	return out
}
