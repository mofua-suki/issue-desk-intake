package jobs

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

func proposalFiles(value any) []string {
	list, ok := value.([]any)
	if !ok || len(list) == 0 || len(list) > 40 {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range list {
		p, e := normalizePath(fleet.Text(raw))
		if e != nil || p == "*" || strings.HasSuffix(p, "/") {
			return nil
		}
		key := strings.ToUpper(p)
		if !seen[key] {
			out = append(out, p)
			seen[key] = true
		}
	}
	return out
}
func matchesTransferProposal(p map[string]any, source string, expected map[string]any) bool {
	if p == nil || p["source"] != source || p["kind"] != "split" || fleet.List(p["dependsOn"]) == nil || len(fleet.List(p["dependsOn"])) != 0 || splitTitle(fleet.Text(p["title"])) != splitTitle(fleet.Text(expected["title"])) || p["purpose"] != expected["purpose"] || !reflect.DeepEqual(p["acceptance"], expected["acceptance"]) {
		return false
	}
	paths := proposalFiles(expected["changePaths"])
	return paths == nil || reflect.DeepEqual(fleet.Clone(paths), fleet.Object(p["changeScope"])["paths"])
}
func matchesTransferDraft(d, r, expected map[string]any) bool {
	p := fleet.Object(r["proposal"])
	saved := fleet.Object(d["proposal"])
	if d["project"] != r["repo"] || d["provider"] != p["provider"] || saved["source"] != p["source"] || saved["id"] != r["id"] || saved["kind"] != "split" || splitTitle(fleet.Text(d["title"])) != splitTitle(fleet.Text(expected["title"])) {
		return false
	}
	body := fleet.Text(d["body"])
	matches := regexp.MustCompile(`<!-- issue-desk:continuation:([0-9]+) -->`).FindAllStringSubmatch(body, -1)
	generation := 1
	if len(matches) > 0 {
		if value, e := strconv.Atoi(matches[len(matches)-1][1]); e == nil {
			generation = value
		}
	}
	neutral := func(value any) string { return strings.ReplaceAll(fleet.Text(value), "<!--", "&lt;!--") }
	acceptance := []string{}
	for _, raw := range fleet.List(p["acceptance"]) {
		acceptance = append(acceptance, "- "+neutral(raw))
	}
	notice := fmt.Sprintf("- 元Issueの続き（実装由来の分割・第%d世代）です。フォローアップではないので空き時間を待たず、通常の依頼と同じ順番で着手します。まだ大きければ上限（第3世代）までさらに続きへ分けられ、上限の世代からは分割しません。不要なら閉じてください。", generation)
	number := int64(fleet.Number(r["number"]))
	expectedBody := fmt.Sprintf("## 目的\n%s\n\n## 受入条件\n%s\n\n## 確認済み事項\n- 元Issue #%d へ投稿された後続Issueの案（実装由来の分割・案ID %s・版 %v）から、受付AIとの再相談を経ずに受付が起票しました。\n- 今回の実装範囲・受入条件は上記のみです。\n%s\n\n<!-- issue-desk:continuation:%d -->\n\n先行IssueのPRマージ待ち: #%d。すべての先行PRのマージを確認してから開始します。\n<!-- issue-desk-dependencies:[%d] -->", neutral(p["purpose"]), strings.Join(acceptance, "\n"), number, p["id"], p["version"], notice, generation, number, number)
	if body != expectedBody {
		return false
	}
	var scope any
	if paths := proposalFiles(fleet.Object(p["changeScope"])["paths"]); paths != nil {
		scope = map[string]any{"known": true, "paths": paths}
	}
	return reflect.DeepEqual(d["changeScope"], fleet.Clone(scope))
}
func (s *Store) readTransferObservations(ctx context.Context, source string, ids []string) ([]map[string]any, error) {
	if s.options.AppDB == nil {
		return nil, nil
	}
	rows, err := s.options.AppDB.QueryContext(ctx, bindSQL("SELECT json FROM proposals WHERE "+jsonText("json", "$.id")+"IN(?,?)AND "+jsonText("json", "$.repo")+"||'#'||"+jsonText("json", "$.number")+"=? ORDER BY "+rowOrder(false)), ids[0], ids[1], source)
	if err != nil {
		return nil, err
	}
	out, _, err := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{})
	return out, err
}
