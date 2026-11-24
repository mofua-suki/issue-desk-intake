package issueapi

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
)

func TestCloseRequestScreenListExcludesTerminalHistoryWithoutChangingRecords(t *testing.T) {
	f := newFixture(t)
	statuses := []string{"reviewing", "confirm", "closing", "closed", "already_closed", "declined", "expired", "failed"}
	before := map[string]string{}
	for i, status := range statuses {
		id := fmt.Sprintf("%08d-1111-4111-8111-111111111111", i)
		raw, _ := json.Marshal(Object{"id": id, "project": "owner/repo", "number": 1, "status": status, "comment": "保存済みの原文", "notePending": false})
		before[id] = string(raw)
		if _, e := f.app.DB().Exec("INSERT INTO close_requests(id,json) VALUES(?,?)", id, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	rows, e := f.s.CloseRequests(f.ctx)
	if e != nil || len(rows) != 3 {
		t.Fatal(rows, e)
	}
	for _, row := range rows {
		if !closeRequestOpen(row) {
			t.Fatal(row)
		}
	}
	code, value := f.call("GET", "/api/close-requests", "screen", nil)
	if code != 200 || len(fleet.Items(fleet.ObjectOrEmpty(value)["items"])) != 3 {
		t.Fatal(code, value)
	}
	for i, status := range statuses {
		id := fmt.Sprintf("%08d-1111-4111-8111-111111111111", i)
		row, e := f.s.CloseRequest(f.ctx, id)
		if i < 3 {
			if e != nil || row["status"] != status {
				t.Fatal(row, e)
			}
		} else if e == nil || row != nil {
			t.Fatal("終了履歴を一覧へ戻しました", row, e)
		}
		// 個別の監査用読取は終了後も維持する。
		code, value := f.call("GET", "/api/close-requests/"+id, "screen", nil)
		if code != 200 || fleet.ObjectOrEmpty(value)["status"] != status {
			t.Fatal(code, value)
		}
	}
	after := map[string]string{}
	dbrows, e := f.app.DB().Query("SELECT id,json FROM close_requests")
	if e != nil {
		t.Fatal(e)
	}
	defer dbrows.Close()
	for dbrows.Next() {
		var id, raw string
		if e := dbrows.Scan(&id, &raw); e != nil {
			t.Fatal(e)
		}
		after[id] = raw
	}
	if e := dbrows.Err(); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("読取で保存記録が変わりました")
	}
}

func TestCloseRequestListPaginationAndFilters(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("%08d-1111-4111-8111-111111111111", i)
		project, status := "owner/repo", "confirm"
		if i == 0 {
			project = "other/repo"
		}
		if i == 1 {
			status = "closing"
		}
		raw, _ := json.Marshal(Object{"id": id, "project": project, "status": status})
		if _, err := f.app.DB().Exec("INSERT INTO close_requests(id,json) VALUES(?,?)", id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	code, value := f.call("GET", "/api/close-requests?project=owner%2Frepo&status=confirm&limit=200&page=1", "screen", nil)
	page := fleet.ObjectOrEmpty(value)
	if code != 200 || len(fleet.Items(page["items"])) != 200 || page["total"] != float64(203) || page["hasMore"] != true {
		t.Fatal(code, value)
	}
	code, value = f.call("GET", "/api/close-requests?project=owner%2Frepo&status=confirm&limit=200&page=2", "screen", nil)
	page = fleet.ObjectOrEmpty(value)
	if code != 200 || len(fleet.Items(page["items"])) != 3 || page["hasMore"] != false {
		t.Fatal(code, value)
	}
	for _, query := range []string{"limit=0", "limit=201", "page=0", "page=x", "limit="} {
		if code, value = f.call("GET", "/api/close-requests?"+query, "screen", nil); code != 400 {
			t.Fatal(query, code, value)
		}
	}
	if code, value = f.call("GET", "/api/close-requests", "other", nil); code != 403 {
		t.Fatal(code, value)
	}
}
