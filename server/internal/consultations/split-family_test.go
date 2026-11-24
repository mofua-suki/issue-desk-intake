package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func seedFamily(t *testing.T, s *Service, partial bool) {
	t.Helper()
	parent := Record{"id": todoID, "project": "owner/app", "status": "submit_error", "revision": 0, "splitChildren": []any{photoID, sendID}, "splitIssues": []any{Record{}, Record{}}, "messages": []any{Record{"role": "user", "text": "依頼"}}}
	a := Record{"id": photoID, "project": "owner/app", "parentSplit": todoID, "status": "draft", "revision": 0}
	b := Record{"id": sendID, "project": "owner/app", "parentSplit": todoID, "status": "submit_unknown", "attempted": true, "revision": 0}
	if partial {
		a["status"] = "submitted"
		a["issue"] = Record{"number": int64(5), "url": "/issue"}
	} else {
		b["status"] = "draft"
		delete(b, "attempted")
	}
	for _, d := range []Record{parent, a, b} {
		if _, err := s.options.Records.Change(context.Background(), "drafts", text(d["id"]), func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
	}
}
func TestUnfiledSplitFamilyFinishesAndDiscardsAtomically(t *testing.T) {
	for _, action := range []string{"finish", "discard"} {
		t.Run(action, func(t *testing.T) {
			s, _, _ := fixture(t)
			seedFamily(t, s, false)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			parent, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
			if s.refusal(parent, action) != "" {
				t.Fatal("画面から操作できません", s.refusal(parent, action))
			}
			var err error
			if action == "finish" {
				_, err = s.Finish(ctx, todoID, Record{}, p)
			} else {
				_, err = s.Discard(ctx, todoID, Record{}, p)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{todoID, photoID, sendID} {
				d, found, err := s.options.Records.Get(ctx, "drafts", id)
				if err != nil {
					t.Fatal(err)
				}
				if action == "discard" && found || action == "finish" && (!found || d["status"] != "submitted") {
					t.Fatalf("%s: %v", id, d)
				}
			}
		})
	}
}
func TestPartialSplitChildFinishCompletesParentWithoutChangingFiledSibling(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.Discard(ctx, todoID, Record{}, p); err == nil {
		t.Fatal("一部起票済み家族を破棄しました")
	}
	d, err := s.Finish(ctx, sendID, Record{}, p)
	if err != nil || d["finishedWithoutIssue"] != true {
		t.Fatalf("%v %v", d, err)
	}
	parent, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
	sibling, _, _ := s.options.Records.Get(ctx, "drafts", photoID)
	if parent["status"] != "submitted" || record(parent["issue"])["number"] == nil || sibling["revision"].(interface{ String() string }).String() != "0" {
		t.Fatalf("親=%v 兄弟=%v", parent, sibling)
	}
	if s.familyViewRefusal(parent, "resubmit") == "" {
		t.Fatal("終了した子を再送できてしまいます")
	}
}
func TestSplitFamilyRejectsBusyOrForeignChildWithoutPartialMutation(t *testing.T) {
	for _, kind := range []string{"busy", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := fixture(t)
			seedFamily(t, s, false)
			ctx := context.Background()
			if kind == "busy" {
				s.options.Active = func(id string) bool { return id == sendID }
			} else {
				_, err := s.options.Records.Change(ctx, "drafts", sendID, func(_ *sql.Tx, d Record) (Record, error) { d["parentSplit"] = "別の親"; return d, nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen}); err == nil {
				t.Fatal("不整合な家族を終了しました")
			}
			d, _, _ := s.options.Records.Get(ctx, "drafts", photoID)
			if d["status"] != "draft" {
				t.Fatal("兄弟だけを変更しました", d)
			}
		})
	}
}

func TestPartialSplitParentFinishKeepsPostedChildAndSkipsUnfiledChild(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	ctx := context.Background()
	if _, err := s.options.Records.Change(ctx, "drafts", photoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["issue"] = Record{"number": int64(5), "project": "other/repo"}
		return d, nil
	}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen})
	if err != nil || d["status"] != "submitted" || len(fleet.Items(d["issues"])) != 1 {
		t.Fatalf("親=%v エラー=%v", d, err)
	}
	if record(fleet.Items(d["issues"])[0])["project"] != "other/repo" {
		t.Fatal(d)
	}
	child, _, _ := s.options.Records.Get(ctx, "drafts", sendID)
	sibling, _, _ := s.options.Records.Get(ctx, "drafts", photoID)
	if child["status"] != "submitted" || child["finishedWithoutIssue"] != true || sibling["finishedWithoutIssue"] != nil {
		t.Fatalf("未起票=%v 起票済み=%v", child, sibling)
	}
}

func TestUnfiledSplitWithoutPlansFinishesFamily(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, false)
	ctx := context.Background()
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["splitIssues"] = []any{}; return d, nil }); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
	if reason := s.refusal(p, "finish"); reason != "" {
		t.Fatal(reason)
	}
	if _, err := s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{todoID, photoID, sendID} {
		d, found, err := s.options.Records.Get(ctx, "drafts", id)
		if err != nil || !found || d["status"] != "submitted" {
			t.Fatalf("%s: %v %v", id, d, err)
		}
	}
}

func TestLegacySplitFinishRejectsUnauthorizedAndInvalidInstruction(t *testing.T) {
	for _, kind := range []string{"unauthorized", "instruction"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := fixture(t)
			seedFamily(t, s, true)
			ctx := context.Background()
			for _, id := range []string{todoID, photoID, sendID} {
				_, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
					d["owner"] = "token:owner"
					d["createdAt"] = "2026-11-01T02:00:00Z"
					if id == todoID {
						d["splitIssues"] = []any{}
					} else {
						d["status"] = "submitted"
						d["issue"] = Record{"number": 5}
					}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := map[string]string{}
			for _, id := range []string{todoID, photoID, sendID} {
				var raw string
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				before[id] = raw
			}
			token := "owner"
			if kind == "unauthorized" {
				token = "other"
			}
			_, err := s.Finish(ctx, todoID, Record{"instructionAt": "2026-11-01T01:00:00Z"}, auth.Principal{Kind: auth.PC, TokenID: token})
			if err == nil {
				t.Fatal("拒否されなかった")
			}
			for id, want := range before {
				var got string
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&got); err != nil || got != want {
					t.Fatal(id, err)
				}
			}
		})
	}
}

func TestClosedSplitFamilyCannotFinishOrDiscardAnyRecord(t *testing.T) {
	for _, closedID := range []string{todoID, photoID, sendID} {
		for _, target := range []string{todoID, photoID, sendID} {
			for _, action := range []string{"finish", "discard"} {
				t.Run(closedID+target+action, func(t *testing.T) {
					s, _, _ := fixture(t)
					seedFamily(t, s, false)
					ctx := context.Background()
					if _, e := s.options.Records.Change(ctx, "drafts", closedID, func(_ *sql.Tx, d Record) (Record, error) {
						d["status"] = "closed"
						d["cleanup"] = Record{"reason": "本人の明示指示"}
						return d, nil
					}); e != nil {
						t.Fatal(e)
					}
					before := map[string]string{}
					for _, id := range []string{todoID, photoID, sendID} {
						var raw string
						if e := s.options.Records.DB.QueryRowContext(ctx, `SELECT json FROM drafts WHERE id=?`, id).Scan(&raw); e != nil {
							t.Fatal(e)
						}
						before[id] = raw
					}
					d, _, _ := s.options.Records.Get(ctx, "drafts", target)
					if s.refusal(d, action) == "" {
						t.Fatal("表示の操作拒否がない")
					}
					p := auth.Principal{Kind: auth.Screen}
					var e error
					if action == "finish" {
						_, e = s.Finish(ctx, target, Record{}, p)
					} else {
						_, e = s.Discard(ctx, target, Record{}, p)
					}
					if e == nil {
						t.Fatal("closed familyを変更")
					}
					for id, want := range before {
						var got string
						if e = s.options.Records.DB.QueryRowContext(ctx, `SELECT json FROM drafts WHERE id=?`, id).Scan(&got); e != nil || got != want {
							t.Fatal("家族の記録を変更/削除", id, e)
						}
					}
				})
			}
		}
	}
}

func TestClosedFamilyRefusalDoesNotRevealForeignDraft(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, false)
	ctx := context.Background()
	if _, e := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"] = "closed"
		d["owner"] = "token:other"
		return d, nil
	}); e != nil {
		t.Fatal(e)
	}
	_, e := s.changeFamily(ctx, todoID, "discard", Record{}, auth.Principal{Kind: auth.PC, TokenID: "caller"})
	if e == nil || e.Error() == "利用者の指示で終了した相談です。" {
		t.Fatal("所有者の拒否より先に状態を開示", e)
	}
}

func TestLegacySplitParentFinishPreservesChildren(t *testing.T) {
	for _, status := range []string{"draft", "submit_error"} {
		t.Run(status, func(t *testing.T) {
			s, _, _ := fixture(t)
			seedFamily(t, s, true)
			ctx := context.Background()
			for _, id := range []string{todoID, photoID, sendID} {
				_, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
					if id == todoID {
						d["status"] = status
						d["splitIssues"] = []any{}
					} else {
						d["status"] = "submitted"
						d["issue"] = Record{"number": int64(7), "project": "other/repo"}
						if id == sendID {
							d["issue"] = Record{"number": int64(8)}
						}
						d["messages"] = []any{Record{"text": "保存"}}
					}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := map[string]string{}
			var beforeValue string
			for _, id := range []string{photoID, sendID} {
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&beforeValue); err != nil {
					t.Fatal(err)
				}
				before[id] = beforeValue
			}
			parent, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
			if reason := s.refusal(parent, "finish"); reason != "" {
				t.Fatal(reason)
			}
			result, err := s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen})
			if err != nil {
				t.Fatal(err)
			}
			issues := fleet.Items(result["issues"])
			if len(issues) != 2 {
				t.Fatal(result)
			}
			first, _ := integer(record(issues[0])["number"])
			second, _ := integer(record(issues[1])["number"])
			if result["status"] != "submitted" || record(issues[0])["project"] != "other/repo" || first != 7 || record(issues[1])["project"] != "owner/app" || second != 8 {
				t.Fatal(result)
			}
			for id, want := range before {
				var got string
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&got); err != nil || got != want {
					t.Fatal(id, err)
				}
			}
			if _, err = s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen}); err == nil {
				t.Fatal("再実行を許可")
			}
		})
	}
}

func TestLegacySplitRejectsInvalidChildrenAtomically(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "foreign", "unfiled", "parent busy", "child busy"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := fixture(t)
			seedFamily(t, s, true)
			ctx := context.Background()
			for _, id := range []string{todoID, photoID, sendID} {
				_, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
					if id == todoID {
						d["splitIssues"] = []any{}
						if kind == "duplicate" {
							d["splitChildren"] = []any{photoID, photoID}
						}
						if kind == "missing" {
							d["splitChildren"] = []any{photoID, "missing"}
						}
					} else {
						d["status"] = "submitted"
						d["issue"] = Record{"number": int64(5)}
						if id == sendID && kind == "foreign" {
							d["parentSplit"] = "other"
						}
						if id == sendID && kind == "unfiled" {
							delete(d, "issue")
							d["status"] = "draft"
						}
					}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "parent busy" {
				s.options.Active = func(id string) bool { return id == todoID }
			}
			if kind == "child busy" {
				s.options.Active = func(id string) bool { return id == sendID }
			}
			before := map[string]string{}
			for _, id := range []string{todoID, photoID, sendID} {
				var raw string
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				before[id] = raw
			}
			parent, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
			if s.refusal(parent, "finish") == "" {
				t.Fatal("操作を表示")
			}
			if _, err := s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen}); err == nil {
				t.Fatal("終了を許可")
			}
			for id, want := range before {
				var got string
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&got); err != nil || got != want {
					t.Fatal(id, err)
				}
			}
		})
	}
}

func TestLegacySplitReadFailureLeavesParentAndSiblingUnchanged(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	ctx := context.Background()
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["splitIssues"] = []any{}; return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.options.Records.DB.ExecContext(ctx, "UPDATE drafts SET json=? WHERE id=?", "{", sendID); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, id := range []string{todoID, photoID, sendID} {
		var raw string
		if err = s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		before[id] = raw
	}
	parent, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
	if s.refusal(parent, "finish") == "" {
		t.Fatal("読取失敗で操作を表示")
	}
	if _, err = s.Finish(ctx, todoID, Record{}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("読取失敗で終了")
	}
	for id, want := range before {
		var got string
		if err = s.options.Records.DB.QueryRowContext(ctx, "SELECT json FROM drafts WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatal(id, err)
		}
	}
}

func TestNestedFiledChildAllowsIndividualFinishAndParentCompletion(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	ctx := context.Background()
	_, err := s.options.Records.Change(ctx, "drafts", photoID, func(_ *sql.Tx, d Record) (Record, error) {
		delete(d, "issue")
		d["issues"] = []any{Record{"number": 5, "project": "other/repo"}, Record{"number": 6}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.options.Records.Get(ctx, "drafts", photoID)
	_, err = s.Finish(ctx, sendID, Record{}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
	after, _, _ := s.options.Records.Get(ctx, "drafts", photoID)
	if !reflect.DeepEqual(before, after) || parent["status"] != "submitted" || len(fleet.Items(parent["issues"])) != 2 || record(fleet.Items(parent["issues"])[1])["project"] != "owner/app" {
		t.Fatalf("parent=%v before=%v after=%v", parent, before, after)
	}
}
