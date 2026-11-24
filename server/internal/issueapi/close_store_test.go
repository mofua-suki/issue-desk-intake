package issueapi

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
)

// closeStoreRows は状態・再開待ちの形・PostgreSQLがjsonbとして読めない行を並べる。
// 並びは保存順で、読取は新しい順（逆順）に返す。
var closeStoreRows = []struct{ id, json string }{
	{"reviewing", `{"id":"reviewing","status":"reviewing","project":"owner/repo","number":1}`},
	{"confirm", `{"id":"confirm","status":"confirm","project":"owner/repo","number":1}`},
	{"closing", `{"id":"closing","status":"closing","project":"other/repo","number":2}`},
	{"closed", `{"id":"closed","status":"closed","project":"owner/repo","number":1,"notePending":false,"noteAttempted":true,"notePosted":true}`},
	{"closed-other", `{"id":"closed-other","status":"closed","project":"other/repo","number":1,"notePending":false}`},
	{"closed-number", `{"id":"closed-number","status":"closed","project":"owner/repo","number":2,"notePending":false}`},
	{"note-pending", `{"id":"note-pending","status":"closed","project":"owner/repo","number":1,"notePending":true}`},
	{"note-attempted", `{"id":"note-attempted","status":"closed","project":"owner/repo","number":1,"noteAttempted":true}`},
	{"note-posted-text", `{"id":"note-posted-text","status":"closed","project":"owner/repo","number":1,"noteAttempted":true,"notePosted":"true"}`},
	{"note-posted", `{"id":"note-posted","status":"closed","project":"owner/repo","number":1,"noteAttempted":true,"notePosted":true}`},
	{"pr-failed", `{"id":"pr-failed","status":"closed","project":"owner/repo","number":1,"notePending":false,"prClose":{"result":"failed"}}`},
	{"recovery", `{"id":"recovery","status":"closed","project":"owner/repo","number":1,"notePending":false,"recoveryPending":true}`},
	{"declined", `{"id":"declined","status":"declined","project":"owner/repo","number":1,"recoveryPending":true}`},
	{"expired", `{"id":"expired","status":"expired","project":"owner/repo","number":1}`},
	{"failed", `{"id":"failed","status":"failed","project":"owner/repo","number":1}`},
	{"already", `{"id":"already","status":"already_closed","project":"owner/repo","number":1}`},
	{"status-number", `{"id":"status-number","status":1,"project":"owner/repo","number":1}`},
	// \u0000 はGoでは復号できるが、PostgreSQLのjsonbは受け付けない。
	{"nul-confirm", `{"id":"nul-confirm","status":"confirm","project":"owner/repo","number":1,"comment":"a\u0000b"}`},
	{"nul-closed", `{"id":"nul-closed","status":"closed","project":"owner/repo","number":1,"notePending":true,"comment":"a\u0000b"}`},
	{"nul-declined", `{"id":"nul-declined","status":"declined","project":"owner/repo","number":1,"comment":"a\u0000b"}`},
}

func closeStoreIDs(records []Object, keep func(Object) bool) []string {
	ids := []string{}
	for _, r := range records {
		if keep(r) {
			ids = append(ids, fleet.Text(r["id"]))
		}
	}
	return ids
}

func TestCloseRecordFiltersKeepEveryRecordTheCallersUsePostgres(t *testing.T) {
	f := newFixture(t)
	for _, row := range closeStoreRows {
		if _, e := f.app.DB().Exec("INSERT INTO close_requests(id,json) VALUES(?,?)", row.id, row.json); e != nil {
			t.Fatal(e)
		}
	}
	all := func(Object) bool { return true }
	recovery := func(r Object) bool {
		return oneOf(fleet.Text(r["status"]), "reviewing", "closing") || r["status"] == "closed" && closeRecoveryPending(r)
	}
	history := func(r Object) bool {
		return r["project"] == "owner/repo" && int64Number(r["number"]) == 1 && r["status"] == "closed"
	}
	cases := []struct {
		name   string
		filter closeFilter
		keep   func(Object) bool
		// read は復号する行、want はそのうち呼び元の判定が通す行（どちらも新しい順）。
		read, want []string
	}{
		{"open", closeOpenFilter(), closeRequestOpen,
			[]string{"nul-declined", "nul-closed", "nul-confirm", "closing", "confirm", "reviewing"},
			[]string{"nul-confirm", "closing", "confirm", "reviewing"}},
		{"recovery", closeRecoveryFilter(), recovery,
			[]string{"nul-declined", "nul-closed", "nul-confirm", "recovery", "pr-failed", "note-posted-text", "note-attempted", "note-pending", "closing", "reviewing"},
			[]string{"nul-closed", "recovery", "pr-failed", "note-posted-text", "note-attempted", "note-pending", "closing", "reviewing"}},
		{"history", closeHistoryFilter("owner/repo"), history,
			[]string{"nul-declined", "nul-closed", "nul-confirm", "recovery", "pr-failed", "note-posted", "note-posted-text", "note-attempted", "note-pending", "closed-number", "closed"},
			[]string{"nul-closed", "recovery", "pr-failed", "note-posted", "note-posted-text", "note-attempted", "note-pending", "closed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, e := f.s.closeRecords(f.ctx, tc.filter)
			if e != nil {
				t.Fatal(e)
			}
			if got := closeStoreIDs(records, all); !reflect.DeepEqual(got, tc.read) {
				t.Errorf("復号した行\n got %v\nwant %v", got, tc.read)
			}
			if got := closeStoreIDs(records, tc.keep); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("呼び元が使う行\n got %v\nwant %v", got, tc.want)
			}
		})
	}
	open, e := f.s.CloseRequests(f.ctx)
	if e != nil || !reflect.DeepEqual(closeStoreIDs(open, all), cases[0].want) {
		t.Fatalf("一覧: %v %v", closeStoreIDs(open, all), e)
	}
	detail := Object{}
	if e := f.s.detailCloseHistory(f.ctx, "owner/repo", 1, detail); e != nil {
		t.Fatal(e)
	}
	ids := []string{}
	for _, item := range fleet.Items(detail["closeHistory"]) {
		ids = append(ids, fleet.Text(fleet.ObjectOrEmpty(item)["id"]))
	}
	if !reflect.DeepEqual(ids, cases[2].want) {
		t.Fatalf("閉じた記録: %v", ids)
	}
}

// 壊れた行・オブジェクトでない行は、どの絞り込みでも全件を読んでいたときと同じ結果にする。
func TestCloseRecordFiltersKeepUnreadableRowsPostgres(t *testing.T) {
	filters := map[string]closeFilter{"open": closeOpenFilter(), "recovery": closeRecoveryFilter(), "history": closeHistoryFilter("owner/repo")}
	for _, raw := range []string{`{"status":"declined"`, `[1]`, `"closed"`, `1`} {
		f := newFixture(t)
		if _, e := f.app.DB().Exec("INSERT INTO close_requests(id,json) VALUES(?,?)", "broken", raw); e != nil {
			t.Fatal(e)
		}
		for name, filter := range filters {
			if records, e := f.s.closeRecords(f.ctx, filter); e == nil {
				t.Errorf("%s %s: 復号できない行を読み飛ばしました: %v", name, raw, records)
			}
		}
	}
	f := newFixture(t)
	if _, e := f.app.DB().Exec("INSERT INTO close_requests(id,json) VALUES(?,?)", "null", "null"); e != nil {
		t.Fatal(e)
	}
	for name, filter := range filters {
		records, e := f.s.closeRecords(f.ctx, filter)
		if e != nil || len(records) != 1 || records[0] != nil {
			t.Errorf("%s: %s", name, fmt.Sprint(records, e))
		}
	}
}
