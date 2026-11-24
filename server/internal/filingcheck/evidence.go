package filingcheck

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

func candidates(material Record) map[string]Record {
	out := map[string]Record{"target": obj(material["target"])}
	for _, kind := range []string{"requests", "jobs", "consultations"} {
		for _, item := range fleet.Elements(material[kind]) {
			r := obj(item)
			id := ""
			switch kind {
			case "requests":
				id = fmt.Sprint("request:", fleet.WholeNumber(r["number"]))
			case "jobs":
				id = fmt.Sprint("job:", fleet.WholeNumber(r["issue"]))
			case "consultations":
				id = "consultation:" + fleet.Text(r["id"])
			}
			out[id] = r
			if kind == "requests" && obj(r["job"]) != nil {
				job := clone(r)
				for k, v := range obj(r["job"]) {
					job[k] = v
				}
				out[fmt.Sprint("job:", fleet.WholeNumber(r["number"]))] = job
			}
		}
	}
	return out
}
func (s *Service) expand(ctx context.Context, d, material Record, wanted []any) (Record, error) {
	out := clone(material)
	out["evidenceFinal"] = true
	out["evidence"], out["evidenceMissing"], out["evidenceOutsideCandidates"], out[evidenceOmittedKey] = []any{}, []any{}, []any{}, []any{}
	sources := candidates(material)
	// Replacement rows are deliberately not part of candidates: they may only
	// be resolved here, after the initial material validated them.
	replacements := replacementEvidenceSources(d, material)
	seen := map[string]bool{}
	expanded := 0
	total := 0
	for _, item := range wanted {
		r := obj(item)
		id, kind := fleet.Text(r["candidateId"]), fleet.Text(r["kind"])
		key := id + ":" + kind + ":" + string(raw(r["selection"]))
		if seen[key] {
			continue
		}
		seen[key] = true
		missing := func(reason string) {
			next := "必要な相談を consultation:<相談ID> で指定してください。"
			if strings.HasPrefix(reason, "選択失敗:") || strings.HasPrefix(reason, "目的・受入の節がありません") {
				next = "selection を変更し、別の節または startLine / endLine で行範囲を指定してください。"
			}
			out["evidenceMissing"] = append(fleet.Elements(out["evidenceMissing"]), Record{"candidateId": id, "kind": kind, "reason": reason, "selection": r["selection"], "next": next})
		}
		if kind != "body" && kind != "reference" {
			missing("資料種別が不正です")
			continue
		}
		source := sources[id]
		replacement := source == nil && replacements[id] != nil
		if replacement {
			source = replacements[id]
		}
		outside := source == nil && strings.HasPrefix(id, "consultation:")
		if source == nil && !outside {
			out["evidenceOutsideCandidates"] = append(fleet.Elements(out["evidenceOutsideCandidates"]), Record{"candidateId": id, "kind": kind, "selection": r["selection"]})
			continue
		}
		if id != "target" && (expanded >= 24 || !seen["candidate:"+id] && candidateCount(seen) >= 12) {
			out[evidenceOmittedKey] = append(fleet.Elements(out[evidenceOmittedKey]), Record{"candidateId": id, "kind": kind, "reason": "件数上限", "bytes": 0, "measured": false, "range": "all", "selection": r["selection"]})
			continue
		}
		var full Record
		var reason string
		var otherProject bool
		var err error
		if replacement {
			full, reason, err = s.replacementEvidence(ctx, d, source)
		} else {
			full, reason, otherProject, err = s.evidenceRecord(ctx, d, id, source, outside)
		}
		if err != nil {
			return nil, err
		}
		if reason != "" {
			missing(reason)
			continue
		}
		if otherProject {
			out["evidenceOutsideCandidates"] = append(fleet.Elements(out["evidenceOutsideCandidates"]), Record{"candidateId": id, "kind": kind, "selection": r["selection"]})
			continue
		}
		if id != "target" {
			expanded++
			seen["candidate:"+id] = true
		}
		body, reference := splitReference(fleet.Text(full["body"]))
		evidence := Record{"candidateId": id, "kind": kind, "body": body, "reference": reference, "messages": full["messages"], "acquiredRanges": Record{"body": "all", "reference": "all"}, "omittedMessages": 0}
		if r["selection"] != nil {
			evidence["selection"] = r["selection"]
		}
		selection := obj(r["selection"])
		if outside {
			for _, field := range []string{"title", "status", "changeScope", "issue", "issues"} {
				evidence[field] = full[field]
			}
			if len(selection) == 0 || len(selection) == 3 && selection["body"] == nil && selection["reference"] == nil && selection["messages"] == nil {
				summary := consultationSections(body)
				if summary == "" {
					missing("目的・受入の節がありません。selection で必要な節を指定してください")
					continue
				}
				evidence["body"], evidence["reference"], evidence["messages"] = summary, nil, []any{}
				evidence["acquiredRanges"] = Record{"body": "目的・受入の節", "reference": nil}
			}
		}
		if err := applySelection(evidence, obj(r["selection"]), outside || replacement); err != nil {
			missing("選択失敗: " + err.Error())
			continue
		}
		size, omission, fits := fitEvidence(evidence, r, id, kind, total)
		if omission != nil {
			out[evidenceOmittedKey] = append(fleet.Elements(out[evidenceOmittedKey]), omission)
		}
		if !fits {
			continue
		}
		total += size
		out["evidence"] = append(fleet.Elements(out["evidence"]), evidence)
	}
	return out, nil
}

// evidenceRecord reads the full record behind an evidence request: a consultation outside the
// candidates, or the latest copy of a candidate consultation. reason explains a missing
// record; otherProject marks a consultation of another project.
func (s *Service) evidenceRecord(ctx context.Context, d Record, id string, source Record, outside bool) (full Record, reason string, otherProject bool, err error) {
	if outside {
		if s.options.Records == nil {
			return nil, "相談を取得できません", false, nil
		}
		saved, found, err := s.evidenceReads.Get(ctx, "drafts", strings.TrimPrefix(id, "consultation:"))
		if err != nil {
			return nil, "", false, err
		}
		if !found {
			return nil, "相談が存在しません", false, nil
		}
		project := fleet.Text(d["project"])
		if project == "" || fleet.Text(saved["project"]) == "" || !strings.EqualFold(project, fleet.Text(saved["project"])) {
			return nil, "", true, nil
		}
		return saved, "", false, nil
	}
	full = clone(source)
	if strings.HasPrefix(id, "consultation:") && s.options.Records != nil {
		saved, found, err := s.evidenceReads.Get(ctx, "drafts", fleet.Text(source["id"]))
		if err != nil {
			return nil, "", false, err
		}
		if !found || fleet.WholeNumber(saved["revision"]) != fleet.WholeNumber(source["revision"]) {
			return nil, "比較中に相談が更新されました", false, nil
		}
		full = saved
	}
	return full, "", false, nil
}

// fitEvidence keeps the evidence within the 60000-byte budget, dropping unselected references
// and messages before the body. It returns the evidence size, the omission to record and
// whether the evidence still fits.
func fitEvidence(evidence, r Record, id, kind string, total int) (int, Record, bool) {
	size := len(raw(evidence))
	if size+total > 60000 {
		// 指定の無い参照・発言は、資料の種類によらず本文より先に省く（body だけの指定でも全部は積まない）。
		selected := obj(r["selection"])
		dropped := Record{}
		if selected["reference"] == nil && evidence["reference"] != nil {
			dropped["reference"] = evidence["reference"]
			evidence["reference"] = nil
		}
		if selected["messages"] == nil && len(fleet.Elements(evidence["messages"])) > 0 {
			dropped["messages"] = evidence["messages"]
			evidence["messages"] = []any{}
		}
		if len(dropped) > 0 {
			size = len(raw(evidence))
			if size+total <= 60000 {
				return size, Record{omissionCandidateIDKey: id, "kind": "reference/messages", omissionReasonKey: omissionInputLimitReason, "bytes": len(raw(dropped)), "measured": true, "range": "all"}, true
			}
		}
		if size+total > 60000 {
			return size, Record{omissionCandidateIDKey: id, "kind": kind, omissionBodyOmittedKey: true, omissionReasonKey: omissionInputLimitReason, "bytes": size, "measured": true, "range": "all", "limitBytes": 60000, "selection": r["selection"]}, false
		}
	}
	return size, nil, true
}

// uniqueEvidenceMissing preserves distinct selections and failure reasons.
func uniqueEvidenceMissing(items []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(items))
	for _, item := range items {
		row := obj(item)
		key := string(raw([]any{row["candidateId"], row["kind"], row["selection"], row["reason"]}))
		if !seen[key] {
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}
func candidateCount(seen map[string]bool) int {
	count := 0
	for key := range seen {
		if strings.HasPrefix(key, "candidate:") {
			count++
		}
	}
	return count
}
func splitReference(body string) (string, any) {
	lines := strings.Split(body, "\n")
	// Generated marker comments are escaped in user content. Once the start
	// marker is found, an unfinished fence in the reference cannot hide its end.
	starts, ends := []int{}, []int{}
	fenced := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if len(starts) > 0 && t == "<!-- issue-desk-reference:end -->" {
			ends = append(ends, i)
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		// Prepare escapes user comments and emits this exact wrapper even after an
		// unfinished body fence. A bare marker inside a fence remains user text.
		generated := t == "<!-- issue-desk-reference:start -->" && i+2 < len(lines) && strings.TrimSpace(lines[i+1]) == "<details>" && strings.TrimSpace(lines[i+2]) == "<summary>受付の経緯・根拠（参照）</summary>"
		if fenced && !generated {
			continue
		}
		if t == "<!-- issue-desk-reference:start -->" {
			starts = append(starts, i)
		}
		if t == "<!-- issue-desk-reference:end -->" {
			ends = append(ends, i)
		}
	}
	if len(starts) == 1 && len(ends) == 1 && starts[0] < ends[0] {
		a, b := starts[0], ends[0]
		return strings.Join(append(append([]string{}, lines[:a]...), lines[b+1:]...), "\n"), strings.Join(lines[a:b+1], "\n")
	}
	if len(starts) > 0 || len(ends) > 0 {
		return body, nil
	}
	// Legacy reference details require the exact summary and a complete closing tag.
	fenced = false
	details := 0
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if details == 0 && t == "<details>" && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "<summary>受付の経緯・根拠（参照）</summary>" {
			depth := 1
			inFence := false
			for j := i + 2; j < len(lines); j++ {
				v := strings.TrimSpace(lines[j])
				if strings.HasPrefix(v, "```") || strings.HasPrefix(v, "~~~") {
					inFence = !inFence
					continue
				}
				if inFence {
					continue
				}
				if v == "<details>" {
					depth++
				}
				if v == "</details>" || v == "&lt;/details>" {
					depth--
					if depth == 0 {
						return strings.Join(append(append([]string{}, lines[:i]...), lines[j+1:]...), "\n"), strings.Join(lines[i:j+1], "\n")
					}
				}
			}
			return body, nil
		}
		if t == "<details>" {
			details++
		}
		if (t == "</details>" || t == "&lt;/details>") && details > 0 {
			details--
		}
		if details > 0 {
			continue
		}
		if t == "## 受付の経緯・根拠" || t == "## 受付の経緯・根拠（参照）" {
			j := i + 1
			f := false
			for ; j < len(lines); j++ {
				v := strings.TrimSpace(lines[j])
				if strings.HasPrefix(v, "```") || strings.HasPrefix(v, "~~~") {
					f = !f
					continue
				}
				if !f && strings.HasPrefix(v, "## ") {
					break
				}
			}
			return strings.Join(append(append([]string{}, lines[:i]...), lines[j:]...), "\n"), strings.Join(lines[i:j], "\n")
		}
	}
	return body, nil
}
func applySelection(e Record, selection Record, outside bool) error {
	if len(selection) == 0 {
		return nil
	}
	if outside && (selection["body"] != nil || selection["reference"] != nil || selection["messages"] != nil) {
		for _, key := range []string{"body", "reference", "messages"} {
			if selection[key] == nil {
				e[key] = nil
				if key != "messages" {
					obj(e["acquiredRanges"])[key] = nil
				}
			}
		}
	}
	for key, v := range selection {
		if key != "body" && key != "reference" && key != "messages" {
			return errors.New("不正な範囲指定")
		}
		if v == nil {
			continue
		}
		r := obj(v)
		if r == nil {
			return errors.New("不正な範囲指定")
		}
		if key == "messages" {
			if len(r) != 2 || fleet.WholeNumber(r["start"]) < 1 || fleet.WholeNumber(r["end"]) < fleet.WholeNumber(r["start"]) {
				return errors.New("不正な範囲指定")
			}
			messages := fleet.Elements(e["messages"])
			start, end := int(fleet.WholeNumber(r["start"]))-1, int(fleet.WholeNumber(r["end"]))
			if end > len(messages) {
				return errors.New("不正な範囲指定")
			}
			e["messages"] = messages[start:end]
			e["omittedMessages"] = len(messages) - (end - start)
			continue
		}
		selected, rangeValue, err := selectText(fleet.Text(e[key]), r, key == "body" && !outside)
		if err != nil {
			return err
		}
		e[key] = selected
		obj(e["acquiredRanges"])[key] = rangeValue
	}
	return nil
}
func selectText(source string, rangeValue Record, protect bool) (string, any, error) {
	lines := strings.Split(source, "\n")
	start, end := 0, len(lines)
	if section := fleet.Text(rangeValue["section"]); section != "" {
		if len(rangeValue) != 1 {
			return "", nil, errors.New("不正な範囲指定")
		}
		found := -1
		level := 0
		fenced := false
		for i, line := range lines {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
				fenced = !fenced
				continue
			}
			if fenced {
				continue
			}
			if !strings.HasPrefix(trim, "#") {
				continue
			}
			n := len(trim) - len(strings.TrimLeft(trim, "#"))
			title := strings.TrimSpace(trim[n:])
			if title == section {
				if found >= 0 {
					return "", nil, errors.New("節が重複しています")
				}
				found = i
				level = n
			}
		}
		if found < 0 {
			return "", nil, errors.New("節が存在しません")
		}
		start = found
		fenced = false
		for i := start + 1; i < len(lines); i++ {
			trim := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
				fenced = !fenced
				continue
			}
			if fenced {
				continue
			}
			if strings.HasPrefix(trim, "#") {
				n := len(trim) - len(strings.TrimLeft(trim, "#"))
				if n <= level {
					end = i
					break
				}
			}
		}
	} else {
		if len(rangeValue) != 2 || fleet.WholeNumber(rangeValue["startLine"]) < 1 || fleet.WholeNumber(rangeValue["endLine"]) < fleet.WholeNumber(rangeValue["startLine"]) {
			return "", nil, errors.New("不正な範囲指定")
		}
		start, end = int(fleet.WholeNumber(rangeValue["startLine"]))-1, int(fleet.WholeNumber(rangeValue["endLine"]))
		if end > len(lines) {
			return "", nil, errors.New("不正な範囲指定")
		}
	}
	// A range cannot start or finish inside a fenced block.
	inside := false
	for i, line := range lines {
		if i == start && inside {
			return "", nil, errors.New("コード囲みの途中で切断しています")
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") || strings.HasPrefix(strings.TrimSpace(line), "~~~") {
			inside = !inside
		}
		if i+1 == end && inside {
			return "", nil, errors.New("コード囲みの途中で切断しています")
		}
	}
	chosen := strings.Join(lines[start:end], "\n")
	if protect { // Preserve declared purpose, acceptance, scope, evidence, cautions and dependencies.
		sections := []string{}
		fenced := false
		for i, line := range lines {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
				fenced = !fenced
				continue
			}
			if fenced {
				continue
			}
			if !strings.HasPrefix(trim, "#") {
				continue
			}
			protected := false
			for _, name := range []string{"目的", "受入", "範囲", "確認済", "注意", "依存", "担当", "判断"} {
				protected = protected || strings.Contains(trim, name)
			}
			if !protected {
				continue
			}
			stop := i + 1
			inside := false
			for stop < len(lines) {
				t := strings.TrimSpace(lines[stop])
				if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
					inside = !inside
				} else if !inside && strings.HasPrefix(t, "#") {
					break
				}
				stop++
			}
			part := strings.Join(lines[i:stop], "\n")
			if !strings.Contains(chosen, part) {
				sections = append(sections, part)
			}
		}
		if len(sections) > 0 {
			chosen += "\n\n" + strings.Join(sections, "\n\n")
		}
	}
	return chosen, rangeValue, nil
}

// consultationSections extracts purpose and acceptance sections, including their subsections.
func consultationSections(body string) string {
	lines := strings.Split(body, "\n")
	var result []string
	start, level := -1, 0
	fenced := false
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(trim, "#") {
			continue
		}
		n := len(trim) - len(strings.TrimLeft(trim, "#"))
		if n > 6 || len(trim) <= n || trim[n] != ' ' {
			continue
		}
		if start >= 0 && n <= level {
			result = append(result, strings.Join(lines[start:i], "\n"))
			start = -1
		}
		if start < 0 && (strings.Contains(trim[n:], "目的") || strings.Contains(trim[n:], "受入")) {
			start, level = i, n
		}
	}
	if start >= 0 {
		result = append(result, strings.Join(lines[start:], "\n"))
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}
