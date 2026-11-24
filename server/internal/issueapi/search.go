package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"strings"
	"time"
)

var validStatus = regexp.MustCompile(`^[a-z_]+$`)

func (s *Service) search(ctx context.Context, query Object, p auth.Principal) (Object, error) {
	o, err := s.parseSearchQuery(query, p)
	if err != nil {
		return nil, err
	}
	projections := newSearchProjections()
	ctx = context.WithValue(ctx, searchProjectionKey{}, projections)
	rows, unread, e := s.listFiltered(ctx, o.state, fleet.Text(query["project"]))
	if e != nil {
		return nil, e
	}
	// Unread candidates are always reported. Without a project filter their
	// rows may be missing, so the result is marked incomplete; with a filter the
	// requested project was read (its failure is an error above).
	partial := func(out Object) Object {
		if len(unread) > 0 {
			out["unreadProjects"] = unread
			if fleet.Text(query["project"]) == "" {
				out["incomplete"] = true
			}
		}
		return out
	}
	found := []Object{}
	for _, row := range rows {
		if searchRowMatches(row, query, o.state, o.statuses, o.periods) {
			found = append(found, row)
		}
	}
	if query["countOnly"] == "1" {
		return partial(Object{"total": len(found)}), nil
	}
	sortSearchRows(found, o.sortKey, o.order)
	result := searchPage(found, o.limit, o.page)
	for _, row := range result["issues"].([]Object) {
		if e := projections.apply(row); e != nil {
			return nil, e
		}
		delete(row, "dispatchRecord")
		delete(row, "executionSettings")
	}
	return partial(result), nil
}

// searchOptions は検索の条件。
type searchOptions struct {
	state, sortKey, order string
	limit, page           int64
	statuses              map[string]bool
	periods               map[string]searchPeriod
}

// parseSearchQuery は検索の条件を検査して読む。mine は送信元のPCを host に置き換える。
func (s *Service) parseSearchQuery(query Object, p auth.Principal) (searchOptions, error) {
	if _, has := query["mine"]; has {
		if p.Kind != auth.PC {
			return searchOptions{}, fail(400, "mineはトークン接続でだけ使えます。")
		}
		if query["mine"] != "1" {
			return searchOptions{}, fail(400, "mineは1で指定してください。")
		}
		if query["host"] != nil {
			return searchOptions{}, fail(400, "mineとhostは併用できません。")
		}
		query["host"] = p.Name
	}
	state := fleet.Text(query["state"])
	if state != "" && !oneOf(state, "open", "closed") {
		return searchOptions{}, fail(400, "stateが不正です。")
	}
	sortKey := fleet.Text(query["sort"])
	if sortKey == "" {
		sortKey = "updated"
	}
	if !oneOf(sortKey, "created", "updated", "closed") {
		return searchOptions{}, fail(400, "sortが不正です。")
	}
	order := fleet.Text(query["order"])
	if order == "" {
		order = "desc"
	}
	if !oneOf(order, "asc", "desc") {
		return searchOptions{}, fail(400, "orderが不正です。")
	}
	limit, page, err := searchPagination(query)
	if err != nil {
		return searchOptions{}, err
	}
	if query["countOnly"] != nil && !oneOf(fleet.Text(query["countOnly"]), "0", "1") {
		return searchOptions{}, fail(400, "countOnlyは0か1です。")
	}
	if query["priority"] != nil && !oneOf(fleet.Text(query["priority"]), "high", "normal", "auto") {
		return searchOptions{}, fail(400, "priorityが不正です。")
	}
	if query["prioritySource"] != nil && !oneOf(fleet.Text(query["prioritySource"]), "user", "ai") {
		return searchOptions{}, fail(400, "prioritySourceが不正です。")
	}
	if repo := fleet.Text(query["project"]); repo != "" {
		if e := s.o.Project(repo); e != nil {
			return searchOptions{}, e
		}
	}
	statuses := map[string]bool{}
	if text := fleet.Text(query["status"]); text != "" {
		for _, v := range strings.Split(text, ",") {
			v = strings.TrimSpace(v)
			if v == "waiting" {
				for _, w := range []string{"pending", "queued", "untracked"} {
					statuses[w] = true
				}
			} else if validStatus.MatchString(v) {
				statuses[v] = true
			} else {
				return searchOptions{}, fail(400, "statusが不正です。")
			}
		}
	}
	periods, err := searchPeriods(query)
	if err != nil {
		return searchOptions{}, err
	}
	return searchOptions{state: state, sortKey: sortKey, order: order, limit: limit, page: page, statuses: statuses, periods: periods}, nil
}

// sortSearchRows は検索結果を指定の日時の順に並べる。日時の無い行は後ろに置き、同じ日時は番号・プロジェクトの順にする。
func sortSearchRows(found []Object, sortKey, order string) {
	key := sortKey + "At"
	sort.SliceStable(found, func(i, j int) bool {
		a, b := fleet.Text(found[i][key]), fleet.Text(found[j][key])
		if a == "" || b == "" {
			return a != "" && b == ""
		}
		if a == b {
			an, _ := number(found[i]["number"])
			bn, _ := number(found[j]["number"])
			if an == bn {
				return fleet.Text(found[i]["project"]) < fleet.Text(found[j]["project"])
			}
			return an < bn
		}
		if order == "asc" {
			return a < b
		}
		return a > b
	})
}

var issueTimePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2})(?::(\d{2})(?:\.(\d{1,9}))?)?(Z|[+-]\d{2}:\d{2})?$`)

func issueTime(value string) (time.Time, error) {
	m := issueTimePattern.FindStringSubmatch(value)
	if len(m) == 0 {
		return time.Time{}, fail(400, "日時の書式が不正です。")
	}
	seconds := m[2]
	if seconds == "" {
		seconds = "00"
	}
	zone := m[4]
	if zone == "" {
		zone = "+09:00"
	}
	return time.Parse(time.RFC3339Nano, m[1]+":"+seconds+map[bool]string{true: "." + m[3], false: ""}[m[3] != ""]+zone)
}

func searchPagination(query Object) (int64, int64, error) {
	limit := int64(100)
	if v, has := query["limit"]; has {
		n, ok := number(v)
		if !ok || n > 100 {
			return 0, 0, fail(400, "limitは1〜100です。")
		}
		limit = n
	}
	page := int64(1)
	if v, has := query["page"]; has {
		n, ok := number(v)
		if !ok {
			return 0, 0, fail(400, "pageは正の整数です。")
		}
		page = n
	}
	if page > 9007199254740991/100 {
		return 0, 0, fail(400, "ページ開始位置が大きすぎます。")
	}
	return limit, page, nil
}

func searchPage(found []Object, limit, page int64) Object {
	total := len(found)
	if limit > 0 {
		start := (page - 1) * limit
		if start >= int64(len(found)) {
			found = []Object{}
		} else {
			end := start + limit
			if end > int64(len(found)) {
				end = int64(len(found))
			}
			found = found[start:end]
		}
	}
	for _, row := range found {
		delete(row, "dispatchRecord")
		delete(row, "executionSettings")
	}
	return Object{"total": total, "issues": found, "hasMore": page*limit < int64(total)}
}
