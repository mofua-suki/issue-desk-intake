package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"path"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/references"
)

var referenceNames = map[string]bool{"gitea-api": true, "measurement": true, "open-requests": true, "rework-by-size": true, "intake-skill-details": true, "docs/intake-skill-details.md": true, "desk-numbers": true}

var continuationCursor = regexp.MustCompile(`^[a-f0-9]{64}:[0-9]+:[0-9]+$`)

const changedListReason = "一覧が変更されたため、続きの候補は未確認です。最新のopen-requestsを再要求し、一覧から照合してください。"

const openRequestsUnavailable = "既存依頼・TODOの一覧を取得できませんでした。重複は未確認です。"

func isContinuation(file string, r ReadRequest) bool {
	return file == "open-requests" && r.Mode == "keyword" && continuationCursor.MatchString(r.Keyword)
}

func (m *materials) read(ctx context.Context, item ReadRequest) (Document, bool, error) {
	project, file := fleet.Text(m.draft["project"]), item.File
	if before, after, exists := strings.Cut(file, ":"); exists && projectName.MatchString(before) {
		project, file = before, after
	}
	if project == "" {
		return Document{}, false, errors.New("プロジェクト未定ではowner/repo:パスを指定してください")
	}
	p, err := m.project(ctx, project)
	if err != nil {
		return Document{}, false, err
	}
	if doc, found := m.readBefore(p, project, file, item); found {
		return doc, false, nil
	}
	doc := Document{Request: item, Project: project, Path: file, Version: p.Revision}
	refused := func(reason string, size int) (Document, bool, error) {
		doc.Reason, doc.Size = reason, size
		doc.Text, doc.Next = "", 0
		p.Reads = replacingRead(p.Reads, item, doc)
		return doc, true, m.persist(ctx)
	}
	continuation := isContinuation(file, item)
	var source string
	if file == "repo-index" || file == "repo-search" {
		source, err = m.repoReference(ctx, p, file, item)
	} else if referenceNames[file] {
		if m.service.options.Reference == nil || project != fleet.Text(m.draft["project"]) {
			return refused("reserved", 0)
		}
		var reference Reference
		if continuation {
			if m.service.options.ReferenceContinuation == nil {
				return doc, false, errors.New("重なり候補の続きは利用できません")
			}
			reference, err = m.service.options.ReferenceContinuation(ctx, m.draft, item.Keyword)
		} else {
			reference, err = m.service.options.Reference(ctx, m.draft, file)
		}
		source, doc.Version = reference.Text, reference.Version
	} else {
		if !contextAllowed(file) {
			return refused("denied", 0)
		}
		entry, exists := p.entries[file]
		if !exists {
			for _, candidate := range sortedFiles(p) {
				if contextAllowed(candidate) && strings.HasSuffix(candidate, "/"+file) {
					if len(doc.Candidates) < maxCandidates {
						doc.Candidates = append(doc.Candidates, candidate)
					} else {
						doc.CandidatesOmitted++
					}
				}
			}
			return refused("missing", 0)
		}
		if item.Mode == "full" && entry.Size > maxFile {
			return refused("file", entry.Size)
		}
		if entry.Size > maxSource {
			return refused("source", entry.Size)
		}
		source, err = m.blob(ctx, p, entry)
	}
	if err != nil {
		if continuation && errors.Is(err, references.ErrOpenListChanged) {
			forgetChangedOpenList(p)
			return refused(changedListReason, 0)
		}
		return doc, false, err
	}
	doc.SHA256 = digest(source)
	if continuation {
		doc.Text = source
	} else {
		doc.Text, doc.Next, err = extract(source, item)
	}
	if err != nil {
		return refused("range", len(source))
	}
	doc.Size = len(doc.Text)
	if doc.Size > maxFile {
		return refused("file", doc.Size)
	}
	reads := replacingRead(p.Reads, item, doc)
	// 重複照合に必須の一覧と、その一覧が発行した続きは資料の累計で断らない。
	requiredOpenRequests := file == "open-requests" && (item.Mode == "full" || continuation && listedContinuation(p.Reads, item.Keyword))
	if !requiredOpenRequests && projectBytes(reads)+m.evidenceBytes(project) > maxProject {
		return refused("total", doc.Size)
	}
	p.Reads, p.bytes = reads, projectBytes(reads)
	if file == "open-requests" && item.Mode == "full" {
		m.state.Notice = strings.ReplaceAll(m.state.Notice, openRequestsUnavailable, "")
	}
	return doc, true, m.persist(ctx)
}

// Only cursors provided by the latest full list or an earlier accepted page
// may bypass the project budget. Other valid positions follow the normal limit.
func listedContinuation(reads []Document, cursor string) bool {
	for _, doc := range reads {
		if doc.Path != "open-requests" || doc.Reason != "" {
			continue
		}
		if doc.Request.Mode == "full" && strings.Contains("\n"+doc.Text, "\n  重なり候補の続き: "+cursor+"\n") ||
			isContinuation(doc.Path, doc.Request) && strings.Contains(doc.Text, "\n続き: "+cursor+"\n") {
			return true
		}
	}
	return false
}

// readBefore returns the verified carried evidence or the earlier read that answers the request.
func (m *materials) readBefore(p *projectState, project, file string, item ReadRequest) (Document, bool) {
	for _, doc := range m.evidence.documents {
		if doc.Project == project && doc.Version == p.Revision && doc.Path == file && item.Mode == "lines" && item.Start == doc.Request.Start && item.End == doc.Request.End {
			return doc, true
		}
	}
	for _, prior := range p.Reads {
		if sameRead(prior, project, file, p.Revision, item) && prior.Reason == "" && (prior.Text != "" || prior.Size == 0 && prior.SHA256 != "") {
			return prior, true
		}
	}
	return Document{}, false
}

// sameRead compares resolved pinned files without changing persisted document keys.
func sameRead(doc Document, project, file, revision string, item ReadRequest) bool {
	if doc.Project != project || doc.Path != file {
		return false
	}
	if isContinuation(file, doc.Request) && isContinuation(file, item) {
		return doc.Request.Keyword == item.Keyword
	}
	if referenceNames[file] || file == "repo-index" || file == "repo-search" {
		return requestKey(doc.Request) == requestKey(item)
	}
	if doc.Version != revision {
		return false
	}
	a, b := doc.Request, item
	a.File, b.File = "", ""
	return pinnedRequestKey(a) == pinnedRequestKey(b)
}

// Key pinned file requests by the fields that affect extraction.
func pinnedRequestKey(r ReadRequest) string {
	if r.Mode == "full" {
		r.Start, r.End = 0, 0
	} else if r.Start == 0 {
		r.Start = 1
	}
	if r.Mode != "keyword" {
		r.Keyword, r.Context = "", 0
	}
	return requestKey(r)
}

// forgetChangedOpenList drops the open-request list whose continuation no longer matches and
// marks its continuations unread.
func forgetChangedOpenList(p *projectState) {
	reads := p.Reads[:0]
	for _, old := range p.Reads {
		if old.Path == "open-requests" {
			if !isContinuation(old.Path, old.Request) {
				continue
			}
			old.Text, old.SHA256, old.Version, old.Next, old.Size = "", "", "", 0, 0
			old.Reason = changedListReason
		}
		reads = append(reads, old)
	}
	p.Reads = reads
	p.bytes = projectBytes(reads)
}

// replacingRead returns the reads with doc in place of an earlier read of the same request.
func replacingRead(prior []Document, item ReadRequest, doc Document) []Document {
	reads := []Document{}
	for _, old := range prior {
		if !sameRead(old, doc.Project, doc.Path, doc.Version, item) {
			reads = append(reads, old)
		}
	}
	reads = append(reads, doc)
	return reads
}

func (m *materials) bootstrap(ctx context.Context, project string) error {
	p, err := m.project(ctx, project)
	if err != nil {
		return err
	}
	if m.bootstrapped[p] {
		return nil
	}
	if len(p.Reads) > 0 {
		if err := m.restoreReads(ctx, project, p); err != nil {
			return err
		}
	}
	if len(p.Reads) == 0 || m.state.InitialDocumentsCollecting {
		if err := m.readInitialDocuments(ctx, project, p); err != nil {
			return err
		}
	}
	if m.bootstrapped == nil {
		m.bootstrapped = map[*projectState]bool{}
	}
	m.bootstrapped[p] = true
	return nil
}

// restoreReads reads the carried documents again from the pinned revision and refuses
// contents that changed, keeping unread continuations visible as unread.
func (m *materials) restoreReads(ctx context.Context, project string, p *projectState) error {
	prior := append([]Document{}, p.Reads...)
	priorBytes := p.bytes
	m.restoring = true
	defer func() { m.restoring = false }()
	p.Reads = nil
	for _, doc := range prior {
		if doc.Reason != "" {
			p.Reads = append(p.Reads, doc)
			continue
		}
		restored := doc.Request
		if project != fleet.Text(m.draft["project"]) && !strings.Contains(restored.File, ":") {
			restored.File = project + ":" + restored.File
		}
		read, added, err := m.read(ctx, restored)
		if isContinuation(doc.Path, doc.Request) && (err != nil || read.Reason == "" && doc.SHA256 != "" && doc.SHA256 != read.SHA256) {
			// A cursor is tied to a changing list. Keep the request visible as
			// unread and ask for a fresh list instead of stopping the consultation.
			if err == nil {
				p.Reads = p.Reads[:len(p.Reads)-1]
			}
			doc.Text, doc.SHA256, doc.Version, doc.Next, doc.Size = "", "", "", 0, 0
			doc.Reason = "続きの復元に失敗しました。一覧の変更または取得失敗のため未確認です。open-requestsを再要求し、最新の一覧から照合してください。"
			p.Reads = append(p.Reads, doc)
			continue
		}
		if err != nil {
			p.Reads, p.bytes = prior, priorBytes
			return err
		}
		if !referenceNames[doc.Path] && doc.SHA256 != "" && doc.SHA256 != read.SHA256 {
			p.Reads, p.bytes = prior, priorBytes
			return errors.New("保存した資料の内容と固定版が一致しません")
		}
		// Legacy progress can contain aliases with distinct delivery and session
		// keys. Preserve the original request when restoration reused a read so
		// those keys still identify the body that was actually delivered.
		if !added && !isContinuation(doc.Path, doc.Request) && doc.Version == read.Version && doc.SHA256 != "" && doc.SHA256 == read.SHA256 && sameRead(doc, read.Project, read.Path, read.Version, read.Request) && documentKey(doc) != documentKey(read) {
			read.Request = doc.Request
			p.Reads = append(p.Reads, read)
		}
	}
	p.bytes = projectBytes(p.Reads)
	m.restoring = false
	if err := m.persist(ctx); err != nil {
		p.Reads, p.bytes = prior, priorBytes
		return err
	}
	return nil
}

// readInitialDocuments reads the project's guide and manifest files on the first turn.
func (m *materials) readInitialDocuments(ctx context.Context, project string, p *projectState) error {
	for _, file := range []string{"AGENTS.md", "README.md", "docs/handoff.md", "docs/design.md", "docs/roadmap.md", "package.json", "pyproject.toml", "go.mod"} {
		entry, exists := p.entries[file]
		if !exists {
			continue
		}
		mode := "full"
		if entry.Size > 32000 && file != "AGENTS.md" && (path.Ext(file) == ".md" || path.Ext(file) == ".ts" || path.Ext(file) == ".go") {
			mode = "outline"
		}
		if _, _, err := m.read(ctx, ReadRequest{File: project + ":" + file, Mode: mode, Context: 5}); err != nil {
			return err
		}
	}
	return nil
}

// ensureOpenRequests は、重複照合の一覧をAIの読み足しに頼らずコントローラーが渡す。
// 読み足しの枠が尽きた相談でも照合できるよう、回数には数えない。取得できないときは相談を止めず、未確認として知らせる。
func (m *materials) ensureOpenRequests(ctx context.Context) error {
	if !m.service.options.EagerOpenRequests || m.service.options.Reference == nil || fleet.Text(m.draft["project"]) == "" {
		return nil
	}
	doc, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "full", Context: 5})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return m.updateOpenRequestsNotice(ctx, err != nil || doc.Reason != "")
}

func (m *materials) updateOpenRequestsNotice(ctx context.Context, unavailable bool) error {
	notice := strings.ReplaceAll(m.state.Notice, openRequestsUnavailable, "")
	if unavailable {
		notice += openRequestsUnavailable
	}
	if notice == m.state.Notice {
		return nil
	}
	m.state.Notice = notice
	return m.persist(ctx)
}

func (m *materials) repoReference(ctx context.Context, p *projectState, name string, request ReadRequest) (string, error) {
	if name == "repo-index" {
		return strings.Join(sortedFiles(p), "\n"), nil
	}
	if request.Mode != "keyword" {
		return "", errors.New("repo-searchはkeywordだけで要求してください")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := searchKey{repo: p.Name, revision: p.Revision, keyword: request.Keyword}
	cacheable := sha40.MatchString(p.Revision)
	if cacheable {
		if result, ok := m.service.searches.get(key); ok {
			return result, ctx.Err()
		}
	}
	rows := []string{}
	bytes := 0
	err := m.searchSources(ctx, p, sortedFiles(p), func(file, source string) error {
		if !strings.Contains(source, request.Keyword) {
			return nil
		}
		lineNumber := 0
		for line := range strings.SplitSeq(source, "\n") {
			lineNumber++
			position := strings.Index(line, request.Keyword)
			if position < 0 {
				continue
			}
			prefix := []rune(line[:position])
			if len(prefix) > 100 {
				line = string(prefix[len(prefix)-100:]) + line[position:]
			}
			characters := []rune(line)
			if len(characters) > 500 {
				line = string(characters[:500])
			}
			bytes += len(line) + len(file) + 30
			if bytes > maxSource {
				return errors.New("検索結果が多いため、キーワードを絞ってください")
			}
			rows = append(rows, fmt.Sprintf("%s:%d: %s", file, lineNumber, line))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	result := strings.Join(rows, "\n")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cacheable {
		m.service.searches.put(key, result)
	}
	return result, nil
}
