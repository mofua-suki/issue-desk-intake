package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

func TestPaginationReadsAll3160WhenGiteaClampsPagesTo50(t *testing.T) {
	const total = 3160
	calls := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Query().Get("limit") != "50" {
			t.Fatal("既存Gitea上限と異なるpage要求", r.URL.String())
		}
		page, e := strconv.Atoi(r.URL.Query().Get("page"))
		if e != nil {
			t.Fatal(e)
		}
		start := (page - 1) * 50
		rows := []map[string]any{}
		for i := start; i < start+50 && i < total; i++ {
			rows = append(rows, map[string]any{"number": i + 1})
		}
		raw, _ := json.Marshal(rows)
		res := response(200, string(raw))
		res.Header.Set("X-Total-Count", strconv.Itoa(total))
		return res, nil
	})
	rows, e := c.Pages(context.Background(), "/repos/owner/repo/issues?state=all")
	if e != nil || len(rows) != total || calls != 64 {
		t.Fatal("実環境の50件clampで履歴を打ち切りました", len(rows), calls, e)
	}
	if rows[len(rows)-1]["number"].(json.Number).String() != "3160" {
		t.Fatal("最後のIssueを取得できません")
	}
}
