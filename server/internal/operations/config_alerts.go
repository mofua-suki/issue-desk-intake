package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
)

func (s *Service) temporaryAlerts(ctx context.Context) ([]any, error) {
	out := []any{}
	if s.Fleet == nil {
		return out, nil
	}
	doc, _, e := s.Fleet()
	if e != nil {
		return nil, e
	}
	selected, e := s.expiredTemporarySettings(doc)
	if e != nil {
		return nil, e
	}
	for _, w := range selected {
		r := w.r
		review := fleet.Object(r["testTimeoutReview"])
		out = append(out, map[string]any{"id": "temporary-setting-expired-" + w.host + "-" + fleet.Text(r["name"]) + "-testTimeoutSeconds", "kind": "temporary_setting_expired", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(s.now()), "title": "固定テストの一時的な期限の見直しが必要です", "detail": fmt.Sprintf("ホスト: %s\nリポジトリ: %v\ntestTimeoutSeconds: %v秒\nreviewBy: %v（日本時間0時）\n理由: %s\n直し方: 値を戻して testTimeoutSeconds と testTimeoutReview を外す、または理由を添えて reviewBy を延ばすPRを作成してください。", w.host, r["name"], r["testTimeoutSeconds"], review["reviewBy"], usage.Redact(fleet.Text(review["reason"])))})
	}
	return out, nil
}
func configsAny(v map[string]map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range v {
		out[k] = v
	}
	return out
}

var openAPIPath = regexp.MustCompile(`^api/openapi/[^/]+\.ya?ml$`)

func (s *Service) designDocAlerts(ctx context.Context) ([]any, error) {
	out := []any{}
	if s.Gitea == nil || s.Repositories == nil {
		return out, nil
	}
	repos, e := s.Repositories()
	if e != nil {
		return nil, e
	}
	configured := false
	for _, repo := range repos {
		if repo == fleet.SystemProject {
			configured = true
		}
	}
	if !configured {
		return out, nil
	}
	tree, e := s.systemProjectTree(ctx)
	if e != nil {
		return nil, e
	}
	for _, raw := range tree {
		entry := fleet.Object(raw)
		path := fleet.Text(entry["path"])
		size := fleet.Number(entry["size"])
		if !openAPIPath.MatchString(path) || strings.Contains(path, "\\") || strings.Contains(path, "..") || entry["type"] != "blob" || (entry["mode"] != "100644" && entry["mode"] != "100755") || entry["size"] == nil || size < 0 || math.Trunc(size) != size || size <= 81000 {
			continue
		}
		label := "9割超"
		if size > 90000 {
			label = "上限超過"
		}
		out = append(out, map[string]any{"id": "design-doc-near-limit:" + path, "kind": "design_doc_near_limit", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(s.now()), "title": path + "（" + label + "）", "detail": fmt.Sprintf("%s\n実測: %.0f bytes\n上限: 90000 bytes\n9割: 81000 bytes\n残り: %.0f bytes\n上限は行数ではなく1500行相当のバイト数の代用値です。自動起票はしません。", path, size, 90000-size)})
	}
	return out, nil
}

// temporaryWarning は見直し日を過ぎた一時設定1件。
type temporaryWarning struct {
	host string
	r    map[string]any
	at   time.Time
}

// expiredTemporarySettings は各PCと既定の一時的な固定テストの期限を検査し、見直し日を過ぎたものを選ぶ。
func (s *Service) expiredTemporarySettings(doc fleet.Document) ([]temporaryWarning, error) {
	configs := map[string]map[string]any{"defaults": doc.Defaults}
	for host, c := range doc.Hosts {
		configs[host] = c
	}
	selected := []temporaryWarning{}
	for _, host := range fleet.Keys(configsAny(configs)) {
		c := configs[host]
		if raw, ok := c["repositories"]; ok {
			if _, ok = raw.([]any); !ok {
				return nil, fmt.Errorf("リポジトリ一覧が配列ではありません")
			}
		}
		for _, raw := range fleet.List(c["repositories"]) {
			r := fleet.Object(raw)
			if !fleet.ValidRepo(fleet.Text(r["name"])) {
				return nil, fmt.Errorf("リポジトリ名が不正です")
			}
			seconds, hasValue := r["testTimeoutSeconds"]
			review, hasReview := r["testTimeoutReview"]
			if hasValue != hasReview {
				return nil, fmt.Errorf("一時的な固定テストの期限と見直し期限は両方必要です")
			}
			if !hasReview {
				continue
			}
			n, ok := seconds.(float64)
			v := fleet.Object(review)
			if !ok || n <= 0 || math.Trunc(n) != n || len(v) != 2 || strings.TrimSpace(fleet.Text(v["reason"])) == "" {
				return nil, fmt.Errorf("一時設定の見直しの形が不正です")
			}
			day := fleet.Text(v["reviewBy"])
			at, e := time.ParseInLocation("2006-01-02", day, time.FixedZone("JST", 9*3600))
			if e != nil || at.Format("2006-01-02") != day || strings.HasPrefix(day, "0000") {
				return nil, fmt.Errorf("一時設定の見直し日が不正です")
			}
			if !s.now().Before(at) {
				selected = append(selected, temporaryWarning{host, r, at})
			}
		}
	}
	return selected, nil
}

// systemProjectTree は受付のリポジトリの既定ブランチの先頭にあるファイル一覧を、ページを追って完全に読む。
func (s *Service) systemProjectTree(ctx context.Context) ([]any, error) {
	prefix := "/repos/" + fleet.SystemProject
	repo := map[string]any{}
	if e := s.Gitea.Do(ctx, "GET", prefix, nil, &repo); e != nil {
		return nil, e
	}
	branch := fleet.Text(repo["default_branch"])
	if branch == "" {
		return nil, fmt.Errorf("既定ブランチを読めません")
	}
	head := map[string]any{}
	if e := s.Gitea.Do(ctx, "GET", prefix+"/branches/"+url.PathEscape(branch), nil, &head); e != nil {
		return nil, e
	}
	sha := fleet.Text(fleet.Object(head["commit"])["id"])
	if !revision.MatchString(sha) {
		return nil, fmt.Errorf("固定版を読めません")
	}
	tree := []any{}
	complete := false
	for page := 1; page <= 10; page++ {
		route := prefix + "/git/trees/" + sha + "?recursive=true"
		if page > 1 {
			route += fmt.Sprintf("&page=%d", page)
		}
		listing := map[string]any{}
		if e := s.Gitea.Do(ctx, "GET", route, nil, &listing); e != nil {
			return nil, e
		}
		entries, ok := listing["tree"].([]any)
		if !ok {
			return nil, fmt.Errorf("ファイル一覧を完全に読めません")
		}
		tree = append(tree, entries...)
		if len(tree) > 4000 {
			return nil, fmt.Errorf("ファイル一覧が上限を超えています")
		}
		if listing["truncated"] != true {
			complete = true
			break
		}
		if len(entries) == 0 {
			break
		}
	}
	if !complete {
		return nil, fmt.Errorf("ファイル一覧を完全に読めません")
	}
	return tree, nil
}
