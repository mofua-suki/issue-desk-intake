package consultationai

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strconv"
	"strings"
)

type evidenceDocument struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Text     string `json:"text"`
}
type evidenceFact struct {
	Statement string   `json:"statement"`
	Documents []string `json:"documents"`
}
type evidenceResult struct {
	present   bool
	documents []Document
	sources   []Record
	facts     []evidenceFact
	rejected  []Record
	paths     map[string]bool
}

func (e evidenceResult) input() any {
	if !e.present {
		return nil
	}
	return Record{"sources": e.sources, "facts": e.facts, "unusable": e.rejected, "meaning": "起票側が確認済みとして報告した事実。内容の意味や利用者の判断・承認を証明するものではありません。"}
}

func (m *materials) evidenceBytes(project string) int {
	total := 0
	for _, doc := range m.evidence.documents {
		if doc.Project == project {
			total += doc.Size
		}
	}
	return total
}

func (m *materials) addEvidenceShown(shown, bodies map[string]bool) {
	for _, doc := range m.evidence.documents {
		if shown[documentKey(doc)] && bodyDocument(doc) {
			bodies[doc.Project+":"+doc.Path] = true
		}
	}
}

var evidenceRangeSuffix = regexp.MustCompile(`(?::([0-9]+)(?:-([0-9]+))?|[（(]([0-9]+)(?:-([0-9]+))?行[）)])\s*$`)

func evidenceOrdinaryCovers(doc Document, start, end int, whole bool) bool {
	if doc.Request.Mode == "full" {
		return true
	}
	if whole || start < 1 || end < start {
		return false
	}
	if doc.Request.Mode == "lines" {
		return start >= doc.Request.Start && end <= doc.Request.End
	}
	if doc.Request.Mode == "keyword" {
		seen := map[int]bool{}
		for _, line := range strings.Split(doc.Text, "\n") {
			if prefix, _, ok := strings.Cut(line, ": "); ok {
				if n, err := strconv.Atoi(prefix); err == nil {
					seen[n] = true
				}
			}
		}
		if end-start >= len(seen) {
			return false
		}
		for n := start; n <= end; n++ {
			if !seen[n] {
				return false
			}
		}
		return true
	}
	return false
}

// evidenceRangeProblem keeps a reported range within the body actually delivered.
// Ordinary delivered documents retain their existing review semantics.
func (m *materials) evidenceRangeProblem(d, answer Record, shown map[string]bool, session *sessionState) string {
	for _, value := range fleet.Elements(answer["reviewedFiles"]) {
		original := strings.TrimSpace(fleet.Text(value))
		match := evidenceRangeSuffix.FindStringSubmatch(original)
		whole := match == nil
		file := reviewedFilePath(original)
		if !strings.Contains(file, ":") {
			file = fleet.Text(d["project"]) + ":" + file
		}
		start, end := 0, 0
		if match != nil {
			a, b := match[1], match[2]
			if a == "" {
				a, b = match[3], match[4]
			}
			start, _ = strconv.Atoi(a)
			end = start
			if b != "" {
				end, _ = strconv.Atoi(b)
			}
		}
		ordinary := false
		for _, p := range m.state.Projects {
			for _, doc := range p.Reads {
				key := documentKey(doc)
				covered := evidenceOrdinaryCovers(doc, start, end, whole)
				if p.Name+":"+doc.Path == file && bodyDocument(doc) && covered && (shown[key] || m.state.Delivered[key] || session != nil && session.ID != "" && session.Sent[key]) {
					ordinary = true
				}
			}
		}
		if ordinary {
			continue
		}
		found, covered := false, false
		for _, doc := range m.evidence.documents {
			if doc.Project+":"+doc.Path == file {
				found = true
				covered = covered || !whole && start >= doc.Request.Start && end <= doc.Request.End && end >= start
			}
		}
		if found && !covered {
			return "引継ぎ本文の範囲外は確認していません: " + original
		}
	}
	return ""
}

// verifyReadEvidence uses the same pinned tree and verified blobs as ordinary reads.
// Results are reconstructed on every turn; a declaration or an old delivery key
// alone never creates a document in the current context.
func (m *materials) verifyReadEvidence(ctx context.Context, p *projectState) error {
	m.evidence = evidenceResult{paths: map[string]bool{}}
	raw, ok := m.draft["readEvidence"]
	if !ok || raw == nil {
		return nil
	}
	m.evidence.present = true
	var input struct {
		Version   int                `json:"version"`
		Documents []evidenceDocument `json:"documents"`
		Facts     []evidenceFact     `json:"facts"`
	}
	if json.Unmarshal(marshal(raw), &input) != nil || input.Version != 1 {
		m.evidence.rejected = append(m.evidence.rejected, Record{"reason": "引継ぎ形式が不正です"})
		return nil
	}
	valid := map[string]bool{}
	counts := map[string]int{}
	for _, item := range input.Documents {
		counts[item.ID]++
	}
	used := projectBytes(p.Reads)
	for _, item := range input.Documents {
		source, normalized, reason, err := m.matchEvidenceDocument(ctx, p, item, counts)
		if err != nil {
			return err
		}
		if reason != "" {
			m.evidence.rejected = append(m.evidence.rejected, Record{"id": item.ID, "path": item.Path, "reason": reason})
			continue
		}
		req := ReadRequest{File: p.Name + ":" + item.Path, Mode: "lines", Start: item.Start, End: item.End, Context: 5}
		body, _, err := extract(normalized, req)
		if err != nil || len(body) > maxFile || used+len(body) > maxProject {
			m.evidence.rejected = append(m.evidence.rejected, Record{"id": item.ID, "path": item.Path, "reason": "範囲を配信できません"})
			continue
		}
		used += len(body)
		doc := Document{Request: req, Project: p.Name, Path: item.Path, Version: p.Revision, SHA256: digest(source), Text: body, Size: len(body)}
		m.evidence.documents = append(m.evidence.documents, doc)
		m.evidence.sources = append(m.evidence.sources, Record{"id": item.ID, "project": p.Name, "path": item.Path, "revision": p.Revision, "start": item.Start, "end": item.End})
		m.evidence.paths[item.Path] = true
		valid[item.ID] = true
	}
	m.acceptEvidenceFacts(input.Facts, valid)
	return nil
}

// matchEvidenceDocument compares a carried evidence range with the pinned revision and returns
// the source, its normalized text and the reason it is rejected, if any.
func (m *materials) matchEvidenceDocument(ctx context.Context, p *projectState, item evidenceDocument, counts map[string]int) (string, string, string, error) {
	reason := ""
	var source string
	switch {
	case item.ID == "" || counts[item.ID] != 1 || item.Project != p.Name || item.Revision != p.Revision:
		reason = "プロジェクトまたは固定版が一致しません"
	case !contextAllowed(item.Path) || referenceNames[item.Path] || item.Path == "repo-index" || item.Path == "repo-search":
		reason = "許可された本文資料ではありません"
	case item.Start < 1 || item.End < item.Start || item.Text == "":
		reason = "範囲または本文が不正です"
	default:
		entry, found := p.entries[item.Path]
		if !found {
			reason = "固定版に資料がありません"
		} else {
			var err error
			source, err = m.blob(ctx, p, entry)
			if err != nil {
				if ctx.Err() != nil {
					return "", "", "", ctx.Err()
				}
				reason = "固定版の資料を取得できません"
				m.evidence.paths[item.Path] = true
			}
		}
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	if reason == "" && (item.End > len(lines) || item.Start > len(lines)) {
		reason = "指定範囲が資料にありません"
	}
	if reason == "" && strings.Join(lines[item.Start-1:item.End], "\n") != strings.ReplaceAll(strings.ReplaceAll(item.Text, "\r\n", "\n"), "\r", "\n") {
		reason = "範囲本文が固定版と一致しません"
	}
	return source, normalized, reason, nil
}

// acceptEvidenceFacts keeps the facts whose every source document was verified.
func (m *materials) acceptEvidenceFacts(facts []evidenceFact, valid map[string]bool) {
	for _, fact := range facts {
		good := len(fact.Documents) > 0 && fact.Statement != ""
		for _, id := range fact.Documents {
			good = good && valid[id]
		}
		if good {
			m.evidence.facts = append(m.evidence.facts, fact)
		} else {
			m.evidence.rejected = append(m.evidence.rejected, Record{"reason": "事実の出典をすべて照合できません"})
		}
	}
}
