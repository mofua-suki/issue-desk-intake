package consultationai

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

type materials struct {
	service      *Service
	draft        Record
	state        contextState
	allowed      map[string]bool
	bootstrapped map[*projectState]bool
	restoring    bool
	evidence     evidenceResult
	group        []string
}

func (s *Service) newMaterials(d Record, config map[string]any) (*materials, error) {
	m := &materials{service: s, draft: d, allowed: map[string]bool{}, state: contextState{Projects: map[string]*projectState{}}}
	for _, item := range fleet.Elements(config["repositories"]) {
		m.allowed[fleet.Text(fleet.Object(item)["name"])] = true
	}
	m.group = configuredRepositoryGroup(config, fleet.Text(d["project"]), m.allowed)
	saved := fleet.Object(d["progress"])
	if progress, ok := d["progress"].(contextState); ok {
		if err := json.Unmarshal(marshal(progress), &saved); err != nil {
			return nil, errors.New("保存済みの資料確認を読み取れません")
		}
	}
	if saved != nil {
		copy := map[string]any{}
		for key, value := range saved {
			if key != "pending" {
				copy[key] = value
			}
		}
		if json.Unmarshal(marshal(copy), &m.state) != nil {
			return nil, errors.New("保存済みの資料確認を読み取れません")
		}
		for _, item := range fleet.Elements(saved["pending"]) {
			r, err := request(item)
			if err != nil {
				m.state.Notice = "保存された資料要求が不正です: " + err.Error()
				continue
			}
			m.state.Pending = append(m.state.Pending, r)
		}
		if m.state.Projects == nil {
			m.state.Projects = map[string]*projectState{}
		}
		if project := fleet.Text(d["project"]); project != "" && fleet.Text(saved["revision"]) != "" && m.state.Projects[project] == nil {
			m.state.Projects[project] = legacyProject(project, saved)
		}
		for project, item := range fleet.Object(saved["projects"]) {
			prior := fleet.Object(item)
			if prior != nil && prior["reads"] == nil {
				m.state.Projects[project] = legacyProject(project, prior)
			}
		}
		for _, project := range m.state.Projects {
			if project == nil {
				continue
			}
			for i := range project.Reads {
				doc := &project.Reads[i]
				if len(doc.Candidates) > maxCandidates {
					doc.CandidatesOmitted += len(doc.Candidates) - maxCandidates
					doc.Candidates = doc.Candidates[:maxCandidates]
				}
			}
		}
	}
	if carried := int(fleet.Number(d["carriedRounds"])); carried > m.state.Rounds {
		m.state.Rounds = carried
	}
	return m, nil
}
func legacyProject(name string, prior map[string]any) *projectState {
	p := &projectState{Name: name, Branch: fleet.Text(prior["branch"]), Revision: fleet.Text(prior["revision"])}
	for _, item := range fleet.Elements(prior["files"]) {
		v := fleet.Object(item)
		p.Reads = append(p.Reads, Document{Project: name, Path: fleet.Text(v["path"]), SHA256: fleet.Text(v["sha256"]), Request: ReadRequest{File: fleet.Text(v["path"]), Mode: "full", Context: 5}})
	}
	for _, item := range fleet.Elements(prior["parts"]) {
		v := fleet.Object(item)
		r := ReadRequest{File: fleet.Text(v["path"]), Mode: fleet.Text(v["kind"]), Start: int(fleet.Number(v["start"])), End: int(fleet.Number(v["end"])), Keyword: fleet.Text(v["keyword"]), Context: int(fleet.Number(v["context"]))}
		p.Reads = append(p.Reads, Document{Project: name, Path: r.File, SHA256: fleet.Text(v["sha256"]), Request: r})
	}
	return p
}
func (m *materials) persist(ctx context.Context) error {
	if m.restoring {
		return nil
	}
	progress := m.state
	progress.InitialDocuments = maps.Clone(m.state.InitialDocuments)
	progress.Delivered = maps.Clone(m.state.Delivered)
	progress.Pending = slices.Clone(m.state.Pending)
	progress.Projects = maps.Clone(m.state.Projects)
	for name, project := range m.state.Projects {
		if project == nil {
			continue
		}
		saved := *project
		saved.entries, saved.bytes = nil, 0
		saved.Reads = slices.Clone(project.Reads)
		for index := range saved.Reads {
			saved.Reads[index].Text = ""
			saved.Reads[index].Candidates = slices.Clone(saved.Reads[index].Candidates)
		}
		progress.Projects[name] = &saved
	}
	return m.service.save(ctx, m.draft, Record{"progress": progress, "carriedRounds": m.state.Rounds})
}

var errProjectNotAllowed = errors.New("許可対象外のプロジェクトの資料は読み取れません")

func (m *materials) project(ctx context.Context, name string) (*projectState, error) {
	if !projectName.MatchString(name) || !m.allowed[name] {
		return nil, errProjectNotAllowed
	}
	p := m.state.Projects[name]
	if p == nil {
		var err error
		if p, err = m.pinProject(ctx, name); err != nil {
			return nil, err
		}
	}
	if !sha40.MatchString(p.Revision) || p.Branch == "" {
		return nil, errors.New("資料の固定コミットが不正です")
	}
	if p.entries != nil {
		return p, nil
	}
	if err := m.loadTree(ctx, name, p); err != nil {
		return nil, err
	}
	return p, nil
}

// pinProject pins the project to the current commit of its default branch and saves the pin.
func (m *materials) pinProject(ctx context.Context, name string) (*projectState, error) {
	var info struct {
		Branch string `json:"default_branch"`
	}
	if err := m.service.options.Gitea.Do(ctx, "GET", "/repos/"+name, nil, &info); err != nil {
		return nil, err
	}
	if info.Branch == "" {
		return nil, errors.New("プロジェクトの既定ブランチが不明です")
	}
	var branch struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := m.service.options.Gitea.Do(ctx, "GET", "/repos/"+name+"/branches/"+url.PathEscape(info.Branch), nil, &branch); err != nil {
		return nil, err
	}
	p := &projectState{Name: name, Branch: info.Branch, Revision: branch.Commit.ID, Reads: []Document{}}
	m.state.Projects[name] = p
	if err := m.persist(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// loadTree lists the readable files of the pinned revision. The listing is kept only when complete.
func (m *materials) loadTree(ctx context.Context, name string, p *projectState) error {
	p.entries = nil
	entries, err := m.service.trees.get(ctx, blobKey{name, p.Revision}, func() (map[string]treeEntry, error) {
		return m.fetchTree(ctx, name, p.Revision)
	})
	if err != nil {
		return err
	}
	// Each materials owns its map; permission checks still run before cache use.
	p.entries = entries
	return nil
}

func (m *materials) fetchTree(ctx context.Context, name, revision string) (map[string]treeEntry, error) {
	entries := map[string]treeEntry{}
	total := 0
	for page := 1; page <= 10; page++ {
		var listing struct {
			Tree      []treeEntry `json:"tree"`
			Truncated bool        `json:"truncated"`
		}
		route := "/repos/" + name + "/git/trees/" + revision + "?recursive=true"
		if page > 1 {
			route += fmt.Sprintf("&page=%d", page)
		}
		if err := m.service.options.Gitea.Do(ctx, "GET", route, nil, &listing); err != nil {
			return nil, err
		}
		total += len(listing.Tree)
		if listing.Tree == nil || total > 4000 || listing.Truncated && len(listing.Tree) == 0 {
			return nil, errors.New("資料の一覧を完全に取得できません")
		}
		for _, entry := range listing.Tree {
			if entry.Type == "blob" && entry.Mode != "120000" && entry.Mode != "160000" && contextAllowed(entry.Path) {
				if entry.Size < 0 || !sha40.MatchString(entry.SHA) {
					return nil, errors.New("資料のファイル情報が不正です")
				}
				entries[entry.Path] = entry
			}
		}
		if !listing.Truncated {
			return entries, nil
		}
	}
	return nil, errors.New("資料の一覧を完全に取得できません")
}

func (m *materials) blob(ctx context.Context, p *projectState, entry treeEntry) (string, error) {
	if entry.Size > maxSource {
		return "", errors.New("資料の元ファイルが4000000 bytesを超えています")
	}
	return m.service.blobs.get(ctx, blobKey{p.Name, entry.SHA}, entry.Size, func() (string, error) {
		var blob struct {
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		}
		if err := m.service.options.Gitea.Do(ctx, "GET", "/repos/"+p.Name+"/git/blobs/"+entry.SHA, nil, &blob); err != nil {
			return "", err
		}
		if blob.Encoding != "base64" || len(blob.Content) > maxSource*2 {
			return "", errors.New("資料の内容形式が不正です")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
		if err != nil || len(raw) != entry.Size || !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
			return "", errors.New("資料の内容・大きさ・UTF-8を確認できません")
		}
		hash := sha1.New()
		fmt.Fprintf(hash, "blob %d\x00", len(raw))
		_, _ = hash.Write(raw)
		if hex.EncodeToString(hash.Sum(nil)) != entry.SHA {
			return "", errors.New("固定版の資料と取得した内容が一致しません")
		}
		return string(raw), nil
	})
}
func sortedFiles(p *projectState) []string {
	files := make([]string, 0, len(p.entries))
	for file := range p.entries {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}
