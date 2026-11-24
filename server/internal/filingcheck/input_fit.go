// 確認AIへ渡す資料を入力上限に収める縮小処理（要約の詳細・根拠・候補・参照を順に削り、省略を記録する）。

package filingcheck

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
)

// inputSize は受付AIへ送る入力の大きさ（指示・区切り・スキーマ・確認資料）。
func (s *Service) inputSize(material Record) int {
	size := len(instructions) + len(materialSeparator) + len(s.schema) + len(raw(material))
	if material["evidenceBudgetFinal"] == true {
		size += len(finalBudgetInstructions)
	}
	return size
}

// fitInput は確認資料を入力上限へ収める。従来の縮小（重ならない候補・要約・祖先発言・パスの先頭）で
// 足りないときだけ、要約候補の指示ブロックとジョブ、重ならないパス、読み足した資料、
// 重なりの少ない候補の順に落とし、落とした内容を入力上限の省略として記録する。
// recent は直前の読み足し要求で、読み足した資料はそれ以外を先に落とす。
func (s *Service) fitInput(material Record, recent []any) Record {
	over := func(m Record) bool { return s.inputSize(m) > inputLimit }
	current := reduceNonOverlapping(material)
	// 計測の明細は入力用の写しだけから外し、比較の本文と元の記録は残す。
	for _, candidate := range fleet.Elements(current["consultations"]) {
		delete(obj(candidate), "inputBudget")
	}
	steps := []func(Record) Record{
		reduceCandidates, reduceReferences, reduceSplitBackground, reduceCandidatePaths, dropSummaryDetail, keepOverlappingPaths, projectPreviousRefusal,
		func(m Record) Record { return dropCandidates(m, over) },
		func(m Record) Record { return dropEvidence(m, recent, over) },
	}
	for _, step := range steps {
		if !over(current) {
			break
		}
		current = step(current)
	}
	return current
}

// inputLimitOmittedNote は受付AIへ、落とした部分が未確認であることを示す。
const inputLimitOmittedNote = "入力上限に収めるため受付が省略した部分です。省略部分は未確認で、この省略だけを理由に資料不足で停止しないでください。"

// addInputLimitOmission は省略の種類ごとに1行へまとめて記録する（候補ごとの行で入力を増やさない）。
func addInputLimitOmission(material Record, kind string, ids []string) {
	if len(ids) == 0 {
		return
	}
	rows := fleet.Elements(material[inputLimitOmittedKey])
	list := []any{}
	for _, id := range ids {
		list = append(list, id)
	}
	for index, item := range rows {
		row := clone(obj(item))
		if row["kind"] == kind {
			seen := map[string]bool{}
			merged := []any{}
			for _, id := range append(fleet.Elements(row["candidateIds"]), list...) {
				if !seen[fleet.Text(id)] {
					seen[fleet.Text(id)] = true
					merged = append(merged, id)
				}
			}
			row["candidateIds"], row["count"] = merged, len(merged)
			rows[index] = row
			material[inputLimitOmittedKey] = rows
			return
		}
	}
	material[inputLimitOmittedKey] = append(append([]any{}, rows...), Record{"kind": kind, "reason": "入力上限", "count": len(list), "candidateIds": list, "note": inputLimitOmittedNote})
}

// dropSummaryDetail は要約の候補から指示ブロックとジョブの要約を落とす。受け付け可否（acceptsInstructions）は残す。
func dropSummaryDetail(material Record) Record {
	out := clone(material)
	var ids []string
	for _, key := range []string{"requests", "jobs", "consultations"} {
		items := append([]any{}, fleet.Elements(out[key])...)
		for index, item := range items {
			row := obj(item)
			if row["summaryOnly"] != true {
				continue
			}
			_, blocks := row["instructionBlocks"]
			_, job := row["job"]
			if !blocks && !job {
				continue
			}
			row = clone(row)
			delete(row, "instructionBlocks")
			delete(row, "job")
			items[index] = row
			ids = append(ids, candidateID(key, row))
		}
		if out[key] != nil {
			out[key] = items
		}
	}
	addInputLimitOmission(out, "要約候補の指示ブロック・ジョブ", ids)
	return out
}

// keepOverlappingPaths は要約の候補のパスを、確認対象の変更範囲と重なるものだけにし、全件数を添える。
func keepOverlappingPaths(material Record) Record {
	out := clone(material)
	targetPaths := scopePaths(obj(obj(out["target"])["changeScope"]))
	if len(targetPaths) == 0 {
		return out
	}
	var ids []string
	for _, key := range []string{"requests", "jobs", "consultations"} {
		items := append([]any{}, fleet.Elements(out[key])...)
		for index, item := range items {
			row := obj(item)
			if row["summaryOnly"] != true {
				continue
			}
			row = clone(row)
			changed := false
			for _, field := range []string{"paths", "scope"} {
				list := fleet.Elements(row[field])
				kept := []any{}
				for _, path := range list {
					for _, target := range targetPaths {
						if samePathOrNested(strings.TrimSpace(fleet.Text(path)), target) {
							kept = append(kept, path)
							break
						}
					}
				}
				if len(kept) == len(list) {
					continue
				}
				if _, exists := row[field+"Total"]; !exists {
					row[field+"Total"] = len(list)
				}
				row[field] = kept
				changed = true
			}
			if changed {
				items[index] = row
				ids = append(ids, candidateID(key, row))
			}
		}
		if out[key] != nil {
			out[key] = items
		}
	}
	addInputLimitOmission(out, "要約候補の重ならないパス", ids)
	return out
}

// dropEvidence は読み足した資料を、直前の要求に無いもの・相談・古いものから順に落とす。target は落とさない。
// 落とした資料は evidenceOmitted に bodyOmitted=true・資料入力上限で記録する。
func dropEvidence(material Record, recent []any, over func(Record) bool) Record {
	out := clone(material)
	evidence := append([]any{}, fleet.Elements(out["evidence"])...)
	wanted := map[string]bool{}
	for _, item := range recent {
		r := obj(item)
		wanted[fleet.Text(r["candidateId"])+"\x00"+fleet.Text(r["kind"])] = true
	}
	rank := func(row Record) int {
		id := fleet.Text(row["candidateId"])
		rank := 0
		if wanted[id+"\x00"+fleet.Text(row["kind"])] {
			rank += 2
		}
		if !strings.HasPrefix(id, "consultation:") {
			rank++
		}
		return rank
	}
	for level := 0; level < 4 && over(out); level++ {
		for index := 0; index < len(evidence) && over(out); index++ {
			row := obj(evidence[index])
			if row == nil || fleet.Text(row["candidateId"]) == "target" || rank(row) != level {
				continue
			}
			evidence = append(evidence[:index:index], evidence[index+1:]...)
			index--
			out["evidence"] = evidence
			out[evidenceOmittedKey] = append(append([]any{}, fleet.Elements(out[evidenceOmittedKey])...), Record{omissionCandidateIDKey: row["candidateId"], "kind": row["kind"], omissionBodyOmittedKey: true, omissionReasonKey: omissionInputLimitReason, "bytes": len(raw(row)), "measured": true, "range": "all", "limitBytes": inputLimit, "selection": row["selection"]})
		}
	}
	return out
}

// dropCandidates は候補を、確認対象と重なるパスの少ないものから順に一覧から外し、候補IDと件数だけを残す。
func dropCandidates(material Record, over func(Record) bool) Record {
	out := clone(material)
	targetPaths := scopePaths(obj(obj(out["target"])["changeScope"]))
	type entry struct {
		key   string
		row   Record
		score int
		order int
	}
	var entries []entry
	for _, key := range []string{"requests", "jobs", "consultations"} {
		for _, item := range fleet.Elements(out[key]) {
			row := obj(item)
			if row == nil {
				continue
			}
			score := 0
			for _, field := range []string{"paths", "scope"} {
				for _, path := range fleet.Elements(row[field]) {
					for _, target := range targetPaths {
						if samePathOrNested(strings.TrimSpace(fleet.Text(path)), target) {
							score++
							break
						}
					}
				}
			}
			entries = append(entries, entry{key, row, score, len(entries)})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].score < entries[j].score })
	removed := map[string]bool{}
	var ids []string
	rebuild := func() {
		for _, key := range []string{"requests", "jobs", "consultations"} {
			if out[key] == nil {
				continue
			}
			kept := []any{}
			for _, item := range fleet.Elements(material[key]) {
				if row := obj(item); row == nil || !removed[candidateID(key, row)] {
					kept = append(kept, item)
				}
			}
			out[key] = kept
		}
	}
	for _, e := range entries {
		if !over(out) {
			break
		}
		id := candidateID(e.key, e.row)
		removed[id] = true
		ids = append(ids, id)
		rebuild()
		addInputLimitOmission(out, "候補一覧", ids)
	}
	return out
}

// reduceReferences は比較候補を要約にしても上限を超えるときに、祖先参照の発言を件数だけにする。
// 省略した発言は omittedMessages に数え、未確認として扱う（読み足しの要求先は evidenceRequests）。
func reduceReferences(material Record) Record {
	out := clone(material)
	target := clone(obj(out["target"]))
	if obj(target["decisionContext"]) == nil {
		return out
	}
	dc := clone(obj(target["decisionContext"]))
	refs := []any{}
	omitted := fleet.WholeNumber(dc["omittedMessages"])
	for _, item := range fleet.Elements(dc["references"]) {
		r := clone(obj(item))
		if messages := fleet.Elements(r["messages"]); len(messages) > 0 {
			omitted += int64(len(messages))
			r["messageCount"] = len(messages)
			delete(r, "messages")
		}
		refs = append(refs, r)
	}
	dc["references"] = refs
	dc["omittedMessages"] = omitted
	reduceSiblingBodies(out, dc)
	target["decisionContext"] = dc
	out["target"] = target
	return out
}

// sizeBreakdown は入力上限を超えたときに、どの部分が大きいかを bytes と文字数で示す。
func sizeBreakdown(material Record, total, schemaBytes int) string {
	target := obj(material["target"])
	parts := []struct {
		name  string
		value any
	}{
		{"対象本文", target["body"]}, {"完了計画", target["completionPlanSection"]}, {"分割背景", target["splitBackgroundSection"]}, {"会話と参照", target["decisionContext"]}, {"依頼", material["requests"]},
		{"ジョブ", material["jobs"]}, {"相談", material["consultations"]}, {"置き換え", material["replacements"]},
		{"省略候補", material["omittedCandidateSummaries"]}, {"読み足し", material["evidence"]},
		{"明示依存", material["explicitDependencies"]}, {"ファイル有無", material["filePresence"]},
		{"読み足し省略", material[evidenceOmittedKey]}, {"上限省略", material[inputLimitOmittedKey]},
	}
	out := fmt.Sprintf("内訳(bytes/文字): 合計%d・上限%d・指示%d・区切り%d・スキーマ%d", total, inputLimit, len(instructions), len(materialSeparator), schemaBytes)
	for _, part := range parts {
		b := raw(part.value)
		out += fmt.Sprintf("・%s%d/%d", part.name, len(b), len([]rune(string(b))))
	}
	return out + "。"
}

func reduceCandidates(material Record) Record {
	out := clone(material)
	omitted := []any{}
	for _, key := range []string{"requests", "jobs", "consultations"} {
		items := fleet.Elements(out[key])
		for index, item := range items {
			r := obj(item)
			if r["nonOverlapping"] == true {
				continue
			}
			// 題名とパスは下の要約に残るので、ここへ重ねて入れない。
			omitted = append(omitted, Record{"candidateId": candidateID(key, r), "bytes": len(raw(r)), "measured": true})
			summary := Record{}
			for _, field := range []string{"id", "number", "issue", "title", "paths", "scope", "job", "status", "acceptsInstructions", "instructionBlocks", "revision"} {
				if value, exists := r[field]; exists {
					summary[field] = value
				}
			}
			if job := obj(summary["job"]); job != nil {
				summary["job"] = jobSummary(job)
			}
			summary["summaryOnly"] = true
			items[index] = summary
		}
		out[key] = items
	}
	out["omittedCandidateSummaries"] = omitted
	out["omittedSummaryCandidates"] = len(omitted)
	return out
}

// jobSummary は要約の候補に残すジョブの項目だけを返す。本文や実行記録は読み足し（evidenceRequests）で得る。
func jobSummary(job Record) Record {
	out := Record{}
	for _, field := range []string{"id", "issue", "status", "scope"} {
		if value, exists := job[field]; exists {
			out[field] = value
		}
	}
	return out
}

// summaryPathLimit は入力がなお上限を超えるときに、要約の候補へ残すパスの件数。
const summaryPathLimit = 8

// reduceCandidatePaths は要約にしても上限を超えるときに、各候補のパスを先頭だけ残して全件数を添える。
func reduceCandidatePaths(material Record) Record {
	out := clone(material)
	trim := func(row Record, field string) {
		if list := fleet.Elements(row[field]); len(list) > summaryPathLimit {
			row[field] = append([]any{}, list[:summaryPathLimit]...)
			row[field+"Total"] = len(list)
		}
	}
	for _, key := range []string{"requests", "jobs", "consultations"} {
		items := append([]any{}, fleet.Elements(out[key])...)
		for index, item := range items {
			row := clone(obj(item))
			trim(row, "paths")
			trim(row, "scope")
			if job := obj(row["job"]); job != nil {
				job = clone(job)
				trim(job, "scope")
				row["job"] = job
			}
			items[index] = row
		}
		if items != nil && out[key] != nil {
			out[key] = items
		}
	}
	return out
}

func candidateID(kind string, r Record) string {
	if kind == "requests" {
		return fmt.Sprint("request:", fleet.WholeNumber(r["number"]))
	}
	if kind == "jobs" {
		return fmt.Sprint("job:", fleet.WholeNumber(r["issue"]))
	}
	return "consultation:" + fleet.Text(r["id"])
}
