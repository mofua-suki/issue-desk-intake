package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/auth"
	"testing"
)

func TestDraftExecutionViewUsesCurrentEngineAndTurn(t *testing.T) {
	s, _, _ := fixture(t)
	active := false
	s.options.Active = func(string) bool { return active }
	d := Record{"id": todoID, "status": "thinking", "run": "saved-run"}
	view := func() Record {
		return s.draftView(d, auth.Principal{Kind: auth.Screen}, "", func(Record, string) string { return "不可" })
	}
	if v := view(); v["executionActive"] != false || v["executionScheduled"] != false {
		t.Fatal("保存されたrunだけでは実行中にしない", v)
	}
	active = true
	if v := view(); v["executionActive"] != true {
		t.Fatal("現在所有しているターン", v)
	}
	d["queued"] = true
	if v := view(); v["executionActive"] != false || v["executionScheduled"] != true {
		t.Fatal("実行枠待ちは待機", v)
	}
	d["queued"] = false
	d["retryAt"] = "2026-11-13T00:00:00Z"
	if v := view(); v["executionActive"] != false || v["executionScheduled"] != true {
		t.Fatal("Engineが再試行を待つ", v)
	}
	active = false
	if v := view(); v["executionScheduled"] != false {
		t.Fatal("実行元が消えた予定は未確認", v)
	}
	if d["executionActive"] != nil || d["executionScheduled"] != nil {
		t.Fatal("表示の確認を永続状態へ書かない", d)
	}
}

func TestAcceptedSubmissionExecutionView(t *testing.T) {
	s, _, _ := fixture(t)
	s.options.Submit = func(context.Context, string, Record, auth.Principal) (Record, error) { return nil, nil }
	s.options.SubmissionQueued = func(Record) {}
	e, err := NewEngine(context.Background(), EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, nil }, Submit: func(context.Context, Record) (Record, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	s.BindEngine(e)
	base := Record{"id": todoID, "status": "auto_ready", "revision": 4, "submissionRequest": Record{"revision": 3, "at": "2026-11-23T19:00:00Z", "owner": "", "kind": string(auth.Screen)}}
	cases := []struct {
		name   string
		change func(Record)
		want   bool
	}{
		{"accepted", func(Record) {}, true},
		{"pc", func(d Record) {
			record(d["submissionRequest"])["kind"] = string(auth.PC)
			record(d["submissionRequest"])["owner"] = "token:one"
		}, true},
		{"draft", func(d Record) { d["status"] = "draft" }, false},
		{"failure", func(d Record) { d["status"] = "submit_error" }, false},
		{"unknown", func(d Record) { d["status"] = "submit_unknown" }, false},
		{"attempted", func(d Record) { d["attempted"] = true }, false},
		{"stopped", func(d Record) { d["status"] = "stalled" }, false},
		{"timeout", func(d Record) { d["status"] = "stalled"; d["stallCause"] = "timeout" }, false},
		{"stop flag", func(d Record) { d["_stopped"] = true }, false},
		{"no intent", func(d Record) { delete(d, "submissionRequest") }, false},
		{"bad revision", func(d Record) { record(d["submissionRequest"])["revision"] = 4 }, false},
		{"bad time", func(d Record) { record(d["submissionRequest"])["at"] = "invalid" }, false},
		{"bad owner", func(d Record) {
			record(d["submissionRequest"])["owner"] = "token:"
			record(d["submissionRequest"])["kind"] = string(auth.PC)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := clone(base)
			d["submissionRequest"] = clone(record(base["submissionRequest"]))
			tc.change(d)
			v := s.draftView(d, auth.Principal{Kind: auth.Screen}, "", func(Record, string) string { return "不可" })
			if v["executionScheduled"] != tc.want {
				t.Fatalf("scheduled=%v want %v", v["executionScheduled"], tc.want)
			}
			if d["executionScheduled"] != nil {
				t.Fatal("表示結果が保存状態に混入")
			}
		})
	}
	ctx := context.Background()
	initial := Record{"id": todoID, "project": "owner/app", "status": "draft", "ready": true, "revision": 3, "questions": []any{}}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return initial, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptSubmission(ctx, todoID, Record{"revision": 3}, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.options.Records.ListDraftEntries(ctx, DraftReadScreen)
	if err != nil || len(entries) != 1 {
		t.Fatalf("画面用読取: %v %v", entries, err)
	}
	view := s.DraftScreenViews(entries, auth.Principal{Kind: auth.Screen}, "")[0]
	if view["executionScheduled"] != true || view["submissionRequest"] != nil || view["revision"] != nil {
		t.Fatalf("受理後の画面表示: %v", view)
	}
	s.options.SubmissionQueued = nil
	if s.acceptedSubmissionScheduled(base) {
		t.Fatal("回収経路なし")
	}
	s.options.SubmissionQueued = func(Record) {}
	e.Close()
	if s.acceptedSubmissionScheduled(base) {
		t.Fatal("エンジン停止中")
	}
}
