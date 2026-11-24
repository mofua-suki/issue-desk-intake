package filingcheck

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"unicode"
	"unicode/utf8"
)

func containsToken(source, value string) bool {
	start := 0
	for start <= len(source) {
		offset := strings.Index(source[start:], value)
		if offset < 0 {
			return false
		}
		offset += start
		before, after := rune(0), rune(0)
		if offset > 0 {
			before, _ = utf8.DecodeLastRuneInString(source[:offset])
		}
		if offset+len(value) < len(source) {
			after, _ = utf8.DecodeRuneInString(source[offset+len(value):])
		}
		if !tokenRune(before) && !tokenRune(after) {
			return true
		}
		start = offset + 1
	}
	return false
}
func tokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || strings.ContainsRune("_$./:\\-\u200c\u200d", r)
}

// duplicateEvidenceConfirmed reports whether the duplicate claim quotes the same purpose on
// both sides and names common targets found in both the target and the candidate.
func duplicateEvidenceConfirmed(duplicate, other, material Record, id string) bool {
	purposes := fleet.Text(duplicate["targetPurposeQuote"])
	valid := strings.TrimSpace(purposes) != "" && purposes == fleet.Text(duplicate["candidatePurposeQuote"]) && strings.Contains(fleet.Text(obj(material["target"])["body"]), purposes)
	common := fleet.Elements(duplicate["commonTargets"])
	valid = valid && len(common) > 0 && len(common) <= 24 && other != nil
	sources := []string{fleet.Text(other["body"])}
	bodies := []string{fleet.Text(other["body"])}
	for _, key := range []string{"paths", "scope"} {
		for _, path := range fleet.Elements(other[key]) {
			sources = append(sources, fleet.Text(path))
		}
	}
	for _, path := range fleet.Elements(obj(other["job"])["scope"]) {
		sources = append(sources, fleet.Text(path))
	}
	for _, e := range fleet.Elements(material["evidence"]) {
		row := obj(e)
		if row["candidateId"] == id {
			sources = append(sources, fleet.Text(row["body"]))
			bodies = append(bodies, fleet.Text(row["body"]))
		}
	}
	purposeFound := false
	for _, body := range bodies {
		purposeFound = purposeFound || strings.Contains(body, purposes)
	}
	valid = valid && purposeFound
	for _, item := range common {
		target := obj(item)
		value, kind := fleet.Text(target["value"]), fleet.Text(target["kind"])
		found := false
		for _, source := range sources {
			found = found || containsToken(source, value)
		}
		valid = valid && value != "" && len(value) <= 2048 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n") && (kind == "path" || kind == "setting" || kind == "symbol" || kind == "api") && containsToken(fleet.Text(obj(material["target"])["body"]), value) && found
	}
	return valid
}

func duplicateVerdict(decision, material Record) Record {
	if id := fleet.Text(material["duplicateBodyRequired"]); id != "" && !hasFullDuplicateBody(material, id) && !omittedDuplicateBody(material, id) {
		decision["refusalCategory"] = "other"
		decision["proceed"], decision["duplicate"], decision["reason"], decision["evidenceRequests"] = false, nil, "資料不足: 重複候補 "+id+" の本文を取得できません。本文を相談へ追加し再送してください。", []any{}
		return decision
	}
	duplicate := obj(decision["duplicate"])
	if decision["proceed"] != false || duplicate == nil {
		return decision
	}
	id := fleet.Text(duplicate["candidateId"])
	other := candidates(material)[id]
	if material["evidenceBudgetFinal"] == true && other != nil && !hasFullDuplicateBody(material, id) && (other["nonOverlapping"] == true || strings.TrimSpace(fleet.Text(other["body"])) == "") {
		decision["evidenceRequests"] = append(fleet.Elements(decision["evidenceRequests"]), Record{"candidateId": id, "kind": "body"})
		return decision
	}
	if other != nil && other["nonOverlapping"] == true {
		if material["evidenceFinal"] != true {
			// 要約だけでは重複先を照合できない。候補種別を問わず本文を読む。
			decision["duplicateBodyRequired"] = id
			decision["evidenceRequests"] = append(fleet.Elements(decision["evidenceRequests"]), Record{"candidateId": id, "kind": "body"})
			return decision
		}
		if !hasFullDuplicateBody(material, id) && !omittedDuplicateBody(material, id) {
			decision["refusalCategory"] = "other"
			decision["proceed"], decision["duplicate"], decision["reason"] = false, nil, "資料不足: 重複候補 "+id+" の本文を取得できません。本文を相談へ追加し再送してください。"
			return decision
		}
	}
	if other["acceptsInstructions"] == false {
		decision["proceed"], decision["duplicate"], decision["reason"] = true, nil, "重複先 "+id+" は追加の指示を受け付けないため起票します。"
		return decision
	}
	valid := duplicateEvidenceConfirmed(duplicate, other, material, id)
	if valid {
		return decision
	}
	if other != nil && material["evidenceFinal"] != true && !strings.HasPrefix(id, "job:") {
		requested := false
		for _, item := range fleet.Elements(decision["evidenceRequests"]) {
			requested = requested || obj(item)["candidateId"] == id
		}
		if !requested {
			decision["evidenceRequests"] = append(fleet.Elements(decision["evidenceRequests"]), Record{"candidateId": id, "kind": "body"})
		}
		return decision
	}
	decision["proceed"], decision["duplicate"], decision["reason"] = true, nil, "共通対象と一致する目的の原文引用を両側で確認できないため重複停止を採用しません。"
	return decision
}
func hasFullDuplicateBody(material Record, id string) bool {
	for _, item := range fleet.Elements(material["evidence"]) {
		row := obj(item)
		if row["candidateId"] == id && strings.TrimSpace(fleet.Text(row["body"])) != "" && obj(row["acquiredRanges"])["body"] == "all" {
			return true
		}
	}
	return false
}
func dependencies(decision, material Record) ([]any, error) {
	given, ok := decision["dependsOnIssues"].([]any)
	if !ok {
		return nil, fail("依存指定の一覧が不正です")
	}
	explicit := map[int64]bool{}
	out := []any{}
	for _, item := range fleet.Elements(obj(material["target"])["dependsOnIssues"]) {
		dep := obj(item)
		if dep["source"] == "gitea" && fleet.WholeNumber(dep["number"]) > 0 {
			explicit[fleet.WholeNumber(dep["number"])] = true
			out = append(out, dep)
		}
	}
	candidates := candidates(material)
	proof := map[int64]bool{}
	for _, item := range fleet.Elements(decision["dependencyEvidence"]) {
		e := obj(item)
		id, n := fleet.Text(e["candidateId"]), fleet.WholeNumber(e["number"])
		candidate := candidates[id]
		actual := fleet.WholeNumber(candidate["number"])
		if strings.HasPrefix(id, "job:") {
			actual = fleet.WholeNumber(candidate["issue"])
			if actual == 0 {
				actual = fleet.WholeNumber(candidate["number"])
			}
		}
		if len(e) != 2 || !strings.HasPrefix(id, "request:") && !strings.HasPrefix(id, "job:") || n < 1 || actual != n {
			return nil, fail("依存候補のGitea番号と根拠IDが一致しません。共通番号との対応を確認し再送してください。")
		}
		found := false
		for _, dep := range given {
			found = found || fleet.WholeNumber(obj(dep)["number"]) == n
		}
		if !found {
			return nil, fail("依存根拠に対応する依存指定がありません")
		}
		proof[n] = true
	}
	seen := map[int64]bool{}
	for n := range explicit {
		seen[n] = true
	}
	for _, item := range given {
		d := obj(item)
		n := fleet.WholeNumber(d["number"])
		if len(d) != 2 || d["source"] != "gitea" || n < 1 {
			return nil, fail("依存はsource=giteaと正の整数numberだけを指定してください。")
		}
		if !explicit[n] && !proof[n] {
			return nil, fail(fmt.Sprintf("依存候補 %d の番号と今回の比較資料の組を確認できません。", n))
		}
		if !seen[n] {
			out = append(out, Record{"source": "gitea", "number": n})
			seen[n] = true
		}
	}
	return out, nil
}
