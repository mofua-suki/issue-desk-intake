package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func TestSplitAnswerPersistence(t *testing.T) {
	cases := []struct {
		name, reply  string
		answer, want []any
	}{
		{"empty", "確認しました", []any{}, []any{map[string]any{"title": "元", "body": "内容"}}},
		{"ambiguous", "分割案を取り消すか確認します。", []any{}, []any{map[string]any{"title": "元", "body": "内容"}}},
		{"explicit", "分割案をすべて取り消します。", []any{}, []any{}},
		{"replace", "更新しました", []any{map[string]any{"title": "新"}}, []any{map[string]any{"title": "新"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "相談"}, p); err != nil {
				t.Fatal(err)
			}
			_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
				d["splitIssues"] = []any{Record{"title": "元", "body": "内容"}}
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
				return Record{"reply": tc.reply, "splitIssues": tc.answer}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			e.attempt(ctx, todoID)
			d, err := s.Draft(ctx, todoID, p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fleet.Items(d["splitIssues"]); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestProxySplitSnapshotUndo(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve", true: "cancel"}[cancel], func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			initial := []any{Record{"title": "元"}}
			d := Record{"id": todoID, "project": "owner/app", "status": "thinking", "run": "active", "revision": 2, "splitIssues": initial, "messages": []any{Record{"id": "u", "role": "user"}, Record{"id": "p", "role": "proxy", "advanced": true}}, "questions": []any{}}
			if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
			fields := Record{"splitIssues": []any{}, "_cancelSplitIssues": cancel}
			rows := []Record{{"id": sendID, "project": "owner/app", "draft": todoID, "messageId": "p", "at": "now", "snapshot": Record{"messageCount": 1, "questions": []any{"質問"}, "splitIssues": []any{}}}}
			saved, err := s.options.Records.SaveProxy(ctx, todoID, "active", fields, rows)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(fleet.Items(saved["splitIssues"])); got != map[bool]int{false: 1, true: 0}[cancel] {
				t.Fatal(saved)
			}
			restored, err := s.UndoProxy(ctx, todoID, Record{"messageId": "p"}, auth.Principal{Kind: auth.Screen})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(fleet.Items(restored["splitIssues"])); got != map[bool]int{false: 1, true: 0}[cancel] {
				t.Fatal(restored)
			}
		})
	}
}
