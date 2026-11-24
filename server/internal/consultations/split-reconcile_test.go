package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
	"time"
)

func reconcilePut(t *testing.T, s *Service, id string, update func(Record)) {
	t.Helper()
	_, err := s.options.Records.Change(context.Background(), "drafts", id, func(_ *sql.Tx, d Record) (Record, error) { update(d); return d, nil })
	if err != nil {
		t.Fatal(err)
	}
}
func reconcileGet(t *testing.T, s *Service, id string) Record {
	t.Helper()
	d, ok, err := s.options.Records.Get(context.Background(), "drafts", id)
	if err != nil || !ok {
		t.Fatalf("%s: %v %v", id, ok, err)
	}
	return d
}
func reconcileCall(t *testing.T, s *Service, busy func(string) bool, events *[]Record) bool {
	t.Helper()
	changed, err := s.ReconcileSplitParent(context.Background(), todoID, busy, func(e Record) { *events = append(*events, e) })
	if err != nil {
		t.Fatal(err)
	}
	return changed
}
func reconcileSeed(t *testing.T) *Service {
	t.Helper()
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	reconcilePut(t, s, sendID, func(d Record) { d["status"] = "submitted"; d["issue"] = Record{"number": int64(6)} })
	return s
}
func idleFiling(string) bool { return false }

func TestSplitParentReconcileAfterLateFiling(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	var events []Record
	if reconcileCall(t, s, idleFiling, &events) {
		t.Fatal("未起票の子を完了扱い")
	}
	reconcilePut(t, s, sendID, func(d Record) { d["status"] = "submitted"; d["issue"] = Record{"number": int64(6)} })
	if reconcileCall(t, s, func(id string) bool { return id == sendID }, &events) {
		t.Fatal("起票処理中に更新")
	}
	if !reconcileCall(t, s, idleFiling, &events) {
		t.Fatal("起票後に更新されない")
	}
	p := reconcileGet(t, s, todoID)
	issues := fleet.Items(p["issues"])
	if p["status"] != "submitted" || len(issues) != 2 || len(events) != 1 {
		t.Fatalf("parent=%v events=%v", p, events)
	}
	a, _ := integer(record(issues[0])["number"])
	b, _ := integer(record(issues[1])["number"])
	if a != 5 || b != 6 {
		t.Fatalf("issues=%v", issues)
	}
}
func TestSplitParentReconcileNestedAndFinishedChildren(t *testing.T) {
	s := reconcileSeed(t)
	reconcilePut(t, s, photoID, func(d Record) {
		delete(d, "issue")
		d["issues"] = []any{Record{"number": 7, "project": "other/repo"}, Record{"number": 8}}
	})
	reconcilePut(t, s, sendID, func(d Record) {
		delete(d, "issue")
		d["issues"] = []any{Record{"number": 8, "project": "OWNER/APP"}, Record{"number": 9, "project": "third/repo"}}
	})
	var events []Record
	if !reconcileCall(t, s, idleFiling, &events) {
		t.Fatal("集約されない")
	}
	issues := fleet.Items(reconcileGet(t, s, todoID)["issues"])
	if len(issues) != 3 || text(record(issues[0])["project"]) != "other/repo" || text(record(issues[1])["project"]) != "owner/app" || text(record(issues[2])["project"]) != "third/repo" {
		t.Fatalf("issues=%v", issues)
	}
	s = reconcileSeed(t)
	reconcilePut(t, s, sendID, func(d Record) { delete(d, "issue"); d["finishedWithoutIssue"] = true })
	if !reconcileCall(t, s, idleFiling, &events) || len(fleet.Items(reconcileGet(t, s, todoID)["issues"])) != 1 {
		t.Fatal("起票せず終了した子を扱えない")
	}
	s = reconcileSeed(t)
	reconcilePut(t, s, photoID, func(d Record) { d["issue"] = Record{"number": int64(9007199254740992)} })
	reconcilePut(t, s, sendID, func(d Record) { d["issue"] = Record{"number": int64(9007199254740993)} })
	if !reconcileCall(t, s, idleFiling, &events) {
		t.Fatal("大きなIssue番号を集約できない")
	}
	large := fleet.Items(reconcileGet(t, s, todoID)["issues"])
	if len(large) != 2 {
		t.Fatalf("大きなIssue番号が重複した: %v", large)
	}
	a, _ := integer(record(large[0])["number"])
	b, _ := integer(record(large[1])["number"])
	if a != 9007199254740992 || b != 9007199254740993 {
		t.Fatalf("大きなIssue番号が変化した: %v", large)
	}
}
func TestSplitParentReconcileIncompleteAndInvalidFamily(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *Service)
	}{
		{"unfiled", func(t *testing.T, s *Service) {
			reconcilePut(t, s, sendID, func(d Record) { delete(d, "issue"); d["status"] = "draft" })
		}},
		{"unknown", func(t *testing.T, s *Service) {
			reconcilePut(t, s, sendID, func(d Record) { d["status"] = "submit_unknown" })
		}},
		{"missing", func(t *testing.T, s *Service) {
			_, err := s.options.Records.DB.Exec(bindSQL("DELETE FROM drafts WHERE id=?"), sendID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate", func(t *testing.T, s *Service) {
			reconcilePut(t, s, todoID, func(d Record) { d["splitChildren"] = []any{photoID, photoID} })
		}},
		{"missing-children", func(t *testing.T, s *Service) {
			reconcilePut(t, s, todoID, func(d Record) { delete(d, "splitChildren") })
		}},
		{"parent", func(t *testing.T, s *Service) {
			reconcilePut(t, s, sendID, func(d Record) { d["parentSplit"] = "other" })
		}},
		{"owner", func(t *testing.T, s *Service) {
			reconcilePut(t, s, sendID, func(d Record) { d["owner"] = "token:other" })
		}},
		{"plan", func(t *testing.T, s *Service) {
			reconcilePut(t, s, todoID, func(d Record) { d["splitIssues"] = []any{Record{}} })
		}},
		{"null-plan", func(t *testing.T, s *Service) {
			reconcilePut(t, s, todoID, func(d Record) { d["splitIssues"] = []any{Record{}, nil} })
		}},
		{"empty", func(t *testing.T, s *Service) {
			reconcilePut(t, s, sendID, func(d Record) {
				delete(d, "issue")
				d["issues"] = []any{}
				d["investigations"] = []any{Record{"number": 10}}
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := reconcileSeed(t)
			tc.mutate(t, s)
			before := reconcileGet(t, s, todoID)
			var events []Record
			if reconcileCall(t, s, idleFiling, &events) || !reflect.DeepEqual(before, reconcileGet(t, s, todoID)) || len(events) != 0 {
				t.Fatal("不完全な家族を変更")
			}
		})
	}
}
func TestSplitParentReconcileBusyThenRetry(t *testing.T) {
	for _, kind := range []string{"parent-engine", "child-engine", "parent-filing", "child-filing", "parent-thinking", "child-submitting", "child-auto-ready", "parent-closed", "child-closed"} {
		t.Run(kind, func(t *testing.T) {
			s := reconcileSeed(t)
			var events []Record
			var busyID string
			switch kind {
			case "parent-engine", "parent-filing":
				busyID = todoID
			case "child-engine", "child-filing":
				busyID = sendID
			case "parent-thinking":
				reconcilePut(t, s, todoID, func(d Record) { d["status"] = "thinking" })
			case "child-submitting":
				reconcilePut(t, s, sendID, func(d Record) { d["status"] = "submitting" })
			case "child-auto-ready":
				reconcilePut(t, s, sendID, func(d Record) { d["status"] = "auto_ready" })
			case "parent-closed":
				reconcilePut(t, s, todoID, func(d Record) { d["status"] = "closed" })
			case "child-closed":
				reconcilePut(t, s, sendID, func(d Record) { d["status"] = "closed" })
			}
			engineBusy := kind == "parent-engine" || kind == "child-engine"
			if engineBusy {
				s.options.Active = func(id string) bool { return id == busyID }
			}
			filing := func(id string) bool { return !engineBusy && id == busyID && busyID != "" }
			if reconcileCall(t, s, filing, &events) || len(events) != 0 {
				t.Fatal("処理中を更新")
			}
			if kind == "parent-closed" || kind == "child-closed" {
				return
			}
			s.options.Active = nil
			if kind == "parent-thinking" {
				reconcilePut(t, s, todoID, func(d Record) { d["status"] = "submit_error" })
			}
			if kind == "child-submitting" || kind == "child-auto-ready" {
				reconcilePut(t, s, sendID, func(d Record) { d["status"] = "submitted" })
			}
			if !reconcileCall(t, s, idleFiling, &events) || len(events) != 1 {
				t.Fatal("解除後に更新されない")
			}
		})
	}
}
func TestSplitParentReconcileKeepsGroupLinkFailure(t *testing.T) {
	s := reconcileSeed(t)
	reconcilePut(t, s, todoID, func(d Record) {
		d["_groupLinkGroups"] = []any{[]any{"owner/app", "other/repo"}}
		d["_groupLinksComplete"] = false
		d["error"] = "link failed"
	})
	before := reconcileGet(t, s, todoID)
	var events []Record
	if reconcileCall(t, s, idleFiling, &events) || !reflect.DeepEqual(before, reconcileGet(t, s, todoID)) || len(events) != 0 {
		t.Fatal("相互参照未完了を消去")
	}
}
func TestSplitParentReconcileAtomicAndIdempotent(t *testing.T) {
	t.Run("concurrent child change", func(t *testing.T) {
		s := reconcileSeed(t)
		ctx := context.Background()
		tx, err := s.options.Records.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var pid int
		if err = tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=? FOR UPDATE"), sendID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		before := reconcileGet(t, s, todoID)
		type result struct {
			changed bool
			err     error
			events  int
		}
		done := make(chan result, 1)
		go func() {
			events := 0
			changed, err := s.ReconcileSplitParent(ctx, todoID, idleFiling, func(Record) { events++ })
			done <- result{changed, err, events}
		}()
		// Wait for PostgreSQL to report the blocked reader, then commit the
		// competing change before allowing reconciliation to continue.
		for {
			var blocked bool
			if err = s.options.Records.DB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			select {
			case r := <-done:
				t.Fatalf("did not wait for child: %+v", r)
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}
		if _, err = tx.ExecContext(ctx, bindSQL("UPDATE drafts SET json=jsonb_set(json::jsonb, '{status}', '\"submit_unknown\"')::text WHERE id=?"), sendID); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil || r.changed || r.events != 0 || !reflect.DeepEqual(before, reconcileGet(t, s, todoID)) {
			t.Fatalf("concurrent child change: %+v", r)
		}
	})
	s := reconcileSeed(t)
	reconcilePut(t, s, todoID, func(d Record) {
		d["error"] = "failed"
		d["errorCode"] = "code"
		d["lastFailure"] = Record{"message": "history"}
	})
	before := reconcileGet(t, s, todoID)
	child := reconcileGet(t, s, sendID)
	otherChild := reconcileGet(t, s, photoID)
	var events []Record
	_, err := s.options.Records.DB.Exec("ALTER TABLE drafts ADD CONSTRAINT reconcile_reject CHECK (json::json->>'status' <> 'submitted') NOT VALID")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.ReconcileSplitParent(context.Background(), todoID, idleFiling, func(e Record) { events = append(events, e) })
	if err == nil || changed || !reflect.DeepEqual(before, reconcileGet(t, s, todoID)) || len(events) != 0 {
		t.Fatalf("保存失敗: changed=%v err=%v", changed, err)
	}
	if _, err = s.options.Records.DB.Exec("ALTER TABLE drafts DROP CONSTRAINT reconcile_reject"); err != nil {
		t.Fatal(err)
	}
	if !reconcileCall(t, s, idleFiling, &events) {
		t.Fatal("再試行できない")
	}
	after := reconcileGet(t, s, todoID)
	revision, _ := integer(after["revision"])
	eventRevision, _ := integer(events[0]["revision"])
	if revision != 1 || eventRevision != 1 || text(after["updatedAt"]) == "" {
		t.Fatalf("revision/event: %v %v", after, events)
	}
	if after["error"] != nil || after["errorCode"] != nil || !reflect.DeepEqual(before["lastFailure"], after["lastFailure"]) || !reflect.DeepEqual(before["messages"], after["messages"]) || !reflect.DeepEqual(child, reconcileGet(t, s, sendID)) {
		t.Fatal("履歴または子が変更された")
	}
	if !reflect.DeepEqual(otherChild, reconcileGet(t, s, photoID)) {
		t.Fatal("もう一方の子が変更された")
	}
	restarted := &Service{options: s.options}
	if reconcileCall(t, restarted, idleFiling, &events) || !reflect.DeepEqual(after, reconcileGet(t, s, todoID)) || len(events) != 1 {
		t.Fatal("再実行で重複更新")
	}
}
