package consultationai

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
)

// Handoff references are pinned and bounded before the issue is constructed.
var handoffReference = regexp.MustCompile(`([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:[@:]([a-f0-9]{8,40}))?:([A-Za-z0-9_./-]+):([0-9]+(?:-[0-9]+)?(?:,[0-9]+(?:-[0-9]+)?)*)`)

const handoffLimit = 24000

type handoffItem struct {
	doc      Document
	children []int
}

func (m *materials) prepareHandoff(ctx context.Context, draft, answer Record) (bool, string) {
	if answer["ready"] != true {
		m.handoff = nil
		return false, ""
	}
	type wanted struct {
		repo, revision, path, ranges, original string
		children                               []int
	}
	var wants []wanted
	add := func(text string, children []int) {
		for _, sub := range handoffReference.FindAllStringSubmatch(text, -1) {
			if sub[1] == fleet.Text(draft["project"]) && len(fleet.Elements(answer["splitIssues"])) == 0 {
				continue
			}
			wants = append(wants, wanted{sub[1], sub[2], sub[3], sub[4], sub[0], children})
		}
	}
	add(fleet.Text(draft["body"]), nil)
	for _, raw := range fleet.Elements(draft["messages"]) {
		message := fleet.Object(raw)
		if message["role"] == "user" {
			add(fleet.Text(message["text"]), nil)
		}
	}
	add(fleet.Text(answer["body"]), nil)
	if plan, err := workplan.Parse(answer["executionPlan"]); err == nil && plan != nil {
		for _, r := range plan.Requirements {
			if r.EvidenceKind != "external_check" {
				for _, input := range r.Inputs {
					add(input, r.Children)
				}
			}
		}
	}
	seen := map[string]int{}
	var result []handoffItem
	var problems []string
	total := 0
	projectTotals := map[string]int{}
	for _, w := range wants {
		for _, span := range strings.Split(w.ranges, ",") {
			a, b, ok := strings.Cut(span, "-")
			if !ok {
				b = a
			}
			start, e1 := strconv.Atoi(a)
			end, e2 := strconv.Atoi(b)
			if e1 != nil || e2 != nil || start < 1 || end < start {
				problems = append(problems, w.original+": 行範囲が不正です")
				continue
			}
			key := fmt.Sprintf("%s@%s:%s:%d-%d", w.repo, w.revision, w.path, start, end)
			if idx, ok := seen[key]; ok {
				result[idx].children = mergeHandoffChildren(result[idx].children, w.children)
				continue
			}
			doc, reason := m.fetchHandoff(ctx, w.repo, w.revision, w.path, start, end)
			if reason != "" {
				problems = append(problems, w.original+": "+reason)
				continue
			}
			if _, exists := projectTotals[w.repo]; !exists {
				projectTotals[w.repo] = projectBytes(m.state.Projects[w.repo].Reads) + m.evidenceBytes(w.repo)
			}
			projectTotals[w.repo] += len(doc.Text)
			if projectTotals[w.repo] > maxProject {
				problems = append(problems, w.original+": リポジトリの資料累計が容量上限を超えました")
				continue
			}
			total += len(doc.Text)
			if total > handoffLimit {
				problems = append(problems, w.original+": 資料本文の容量上限を超えました")
				continue
			}
			seen[key] = len(result)
			result = append(result, handoffItem{doc, append([]int(nil), w.children...)})
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err().Error()
	}
	if len(problems) > 0 {
		m.handoff = nil
		return false, strings.Join(problems, "\n")
	}
	m.handoff = result
	fresh := false
	for _, item := range result {
		doc := item.doc
		if !m.state.Delivered[documentKey(doc)] {
			fresh = true
		}
	}
	return fresh, ""
}

func mergeHandoffChildren(a, b []int) []int {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	for _, n := range b {
		exists := false
		for _, v := range a {
			if n == v {
				exists = true
				break
			}
		}
		if !exists {
			a = append(a, n)
		}
	}
	return a
}

func (m *materials) fetchHandoff(ctx context.Context, repo, revision, file string, start, end int) (Document, string) {
	if !m.allowed[repo] || !contextAllowed(file) {
		return Document{}, "読取が許可されていません"
	}
	p, err := m.project(ctx, repo)
	if err != nil {
		return Document{}, "リポジトリが存在しないか、アクセスできません"
	}
	if revision != "" && !strings.HasPrefix(p.Revision, revision) {
		var commit struct {
			SHA string `json:"sha"`
		}
		if err := m.service.options.Gitea.Do(ctx, "GET", "/repos/"+repo+"/git/commits/"+revision, nil, &commit); err != nil || !sha40.MatchString(commit.SHA) || !strings.HasPrefix(commit.SHA, revision) {
			return Document{}, "指定された固定版を取得できません"
		}
		p = &projectState{Name: repo, Branch: p.Branch, Revision: commit.SHA}
		if err := m.loadTree(ctx, repo, p); err != nil {
			return Document{}, "固定版の一覧を取得できません"
		}
	}
	entry, ok := p.entries[file]
	if !ok {
		return Document{}, "固定版に資料がありません"
	}
	if entry.Size > maxSource {
		return Document{}, "資料の元ファイルが容量上限を超えています"
	}
	source, err := m.blob(ctx, p, entry)
	if err != nil {
		return Document{}, "資料の本文を取得できません"
	}
	normalized := strings.ReplaceAll(source, "\r\n", "\n")
	count := 0
	if normalized != "" {
		count = len(strings.Split(strings.TrimSuffix(normalized, "\n"), "\n"))
	}
	if end > count {
		return Document{}, "指定された行範囲がファイルの範囲外です"
	}
	req := ReadRequest{File: repo + ":" + file, Mode: "lines", Start: start, End: end, Context: 5}
	text, _, err := extract(source, req)
	if err != nil {
		return Document{}, "指定された行範囲を取得できません"
	}
	if len(text) > maxFile {
		return Document{}, "資料の範囲が容量上限を超えています"
	}
	return Document{Request: req, Project: repo, Path: file, Version: p.Revision, SHA256: digest(source), Text: text, Size: len(text)}, ""
}

func (m *materials) handoffRecords() []any {
	out := []any{}
	for _, item := range m.handoff {
		d := item.doc
		out = append(out, Record{"project": d.Project, "path": d.Path, "revision": d.Version, "sha256": d.SHA256, "start": d.Request.Start, "end": d.Request.End, "text": d.Text, "textSha256": digest(d.Text), "children": item.children})
	}
	return out
}
