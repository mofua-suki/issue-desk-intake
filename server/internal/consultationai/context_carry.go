package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"path"
	"strings"
)

func (s *Service) restoreCarried(ctx context.Context, d Record, m *materials, session *sessionState) error {
	saved := fleet.Object(d["carriedContext"])
	if saved == nil || d["progress"] != nil {
		return nil
	}
	shadow := copyRecord(d)
	shadow["progress"] = saved
	shadow["carriedRounds"] = 0
	old, err := s.newMaterials(shadow, map[string]any{"repositories": func() []any {
		out := []any{}
		for name := range m.allowed {
			out = append(out, Record{"name": name})
		}
		return out
	}()})
	if err != nil {
		return err
	}
	// nil denotes an old progress record whose initial set is unknown.
	// Preserve that distinction, including a known empty set, across turns.
	m.state.InitialDocuments = old.state.InitialDocuments
	m.state.InitialDocumentsCollecting = old.state.InitialDocumentsCollecting && old.state.InitialDocuments == nil
	for name, prior := range old.state.Projects {
		if prior == nil {
			continue
		}
		if session.ID != "" {
			m.state.Projects[name] = prior
		} else {
			current, err := m.project(ctx, name)
			if err != nil {
				return err
			}
			if current.Revision != prior.Revision {
				continue
			}
			current.Reads = prior.Reads
		}
		if err := m.bootstrap(ctx, name); err != nil {
			return err
		}
	}
	return nil
}
func (m *materials) seed(ctx context.Context) error {
	repo := fleet.Text(m.draft["project"])
	if repo == "" {
		return m.finishSeed(ctx)
	}
	p, err := m.project(ctx, repo)
	if err != nil {
		return err
	}
	named := namedConversationFiles(m.draft)
	missing := []string{}
	for _, file := range named {
		if _, exists := p.entries[file]; !exists && contextAllowed(file) {
			missing = append(missing, file)
		}
	}
	if len(missing) > 0 {
		if p, err = m.repinForNamedFiles(ctx, repo, p, missing); err != nil {
			return err
		}
	}
	if err := m.verifyReadEvidence(ctx, p); err != nil {
		return err
	}
	seedBytes := (maxProject - projectBytes(p.Reads) - m.evidenceBytes(repo)) / 2
	history := 0
	for _, file := range named {
		if m.evidence.paths[file] {
			continue
		}
		entry, exists := p.entries[file]
		if !exists && contextAllowed(file) && history < 5 {
			history++
			var commits []map[string]any
			route := "/repos/" + repo + "/commits?path=" + escape(file) + "&sha=" + p.Revision + "&limit=1"
			if err := m.service.options.Gitea.Do(ctx, "GET", route, nil, &commits); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				m.state.Notice += "履歴未取得: " + file + "。"
			} else if len(commits) > 0 {
				candidate := successor(file, fleet.Elements(commits[0]["files"]), p.entries)
				if candidate != "" {
					m.state.Notice += "改名・統合の履歴からの推定: " + file + " → " + candidate + "。"
					file = candidate
					entry, exists = p.entries[file]
				}
			}
		}
		mode := "full"
		if exists && entry.Size > seedBytes {
			mode = "outline"
		} else if exists {
			seedBytes -= entry.Size
		}
		r := ReadRequest{File: repo + ":" + file, Mode: mode, Context: 5}
		if _, _, err := m.read(ctx, r); err != nil {
			return err
		}
	}
	// Bring explicitly named cross-repository ranges into the first consultation
	// input so the assistant can inspect them before forming a filing plan.
	m.state.HandoffNotice = ""
	if _, problem := m.prepareHandoff(ctx, m.draft, Record{"ready": true, "body": m.draft["body"]}); problem != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.state.HandoffNotice = "必要資料を取得できません: " + problem + "。"
	}
	return m.finishSeed(ctx)
}

// namedConversationFiles lists the file paths named in the body and conversation, once each.
func namedConversationFiles(draft Record) []string {
	sources := []string{fleet.Text(draft["body"])}
	for _, item := range fleet.Elements(draft["messages"]) {
		message := fleet.Object(item)
		if message["role"] == "user" || message["role"] == "assistant" {
			sources = append(sources, fleet.Text(message["text"]))
		}
	}
	combined := strings.Join(sources, "\n")
	named := []string{}
	seen := map[string]bool{}
	for _, file := range namedFiles.FindAllString(combined, -1) {
		if !seen[file] {
			seen[file] = true
			named = append(named, file)
		}
	}
	return named
}

// repinForNamedFiles moves the pin to the current default branch when it contains a named
// file the pinned revision lacks, and notes when the branch cannot be checked.
func (m *materials) repinForNamedFiles(ctx context.Context, repo string, p *projectState, missing []string) (*projectState, error) {
	fresh := &materials{service: m.service, draft: m.draft, allowed: m.allowed, state: contextState{Projects: map[string]*projectState{}}}
	// Inspect a fresh tree without committing a new pin until a named file appears.
	shadow := copyRecord(m.draft)
	shadow["_ephemeral"] = true
	fresh.draft = shadow
	current, refreshErr := fresh.project(ctx, repo)
	if refreshErr != nil {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		m.state.Notice += "名指しのファイルが固定版に無く、既定ブランチの更新も確認できません。不存在と確認済みを区別してください。"
	} else if current.Revision != p.Revision {
		found := false
		for _, file := range missing {
			_, exists := current.entries[file]
			found = found || exists
		}
		if found {
			m.state.Projects[repo] = current
			p = current
			if err := m.bootstrap(ctx, repo); err != nil {
				return p, err
			}
		}
	}
	return p, nil
}

func (m *materials) finishSeed(ctx context.Context) error {
	if m.state.InitialDocumentsCollecting && m.state.InitialDocuments == nil {
		m.state.InitialDocuments = map[string]bool{}
		for _, project := range m.state.Projects {
			for _, doc := range project.Reads {
				m.state.InitialDocuments[documentKey(doc)] = true
			}
		}
		m.state.InitialDocumentsCollecting = false
	}
	return m.persist(ctx)
}
func escape(value string) string { // Query escaping is distinct from repository path validation.
	return url.QueryEscape(value)
}
func successor(old string, changes []any, known map[string]treeEntry) string {
	added, modified := []string{}, []string{}
	for _, item := range changes {
		change := fleet.Object(item)
		name := fleet.Text(change["filename"])
		if _, exists := known[name]; !exists {
			continue
		}
		status := fleet.Text(change["status"])
		if status == "modified" {
			modified = append(modified, name)
		} else if status != "removed" && status != "deleted" {
			added = append(added, name)
		}
	}
	for _, name := range append(append([]string{}, added...), modified...) {
		if path.Base(name) == path.Base(old) {
			return name
		}
	}
	same := []string{}
	for _, name := range added {
		if path.Ext(name) == path.Ext(old) {
			same = append(same, name)
		}
	}
	if len(same) == 1 {
		return same[0]
	}
	if len(same) > 0 {
		return ""
	}
	for _, name := range modified {
		if path.Ext(name) == path.Ext(old) && path.Dir(name) == path.Dir(old) {
			same = append(same, name)
		}
	}
	if len(same) == 1 {
		return same[0]
	}
	return ""
}
