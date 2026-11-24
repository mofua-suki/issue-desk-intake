package consultations

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"slices"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func heldDraft(kind string) Record {
	return Record{
		"usageMeasurementHold": Record{"at": "2026-11-02T22:00:00Z", "step": "受付AIの呼び出し", "diagnostic": Record{"kind": kind, "id": "diag-1"}},
		"consultationBudget":   Record{"calls": float64(5), "weighted": float64(100), "unknownCalls": float64(1), "pendingCall": Record{"id": "p1", "at": "2026-11-02T22:00:00Z"}},
	}
}

func TestReleaseUnmeasuredHoldOnlyForTimeoutKindsAndKeepsTheCallCount(t *testing.T) {
	for _, kind := range []string{"timeout_idle", "timeout_total", "canceled_by_server", "exit_nonzero"} {
		next := heldDraft(kind)
		if !releaseUnmeasuredHold(next, "m1", "2026-11-03T01:00:00Z") {
			t.Fatalf("%s の保留を解除しません", kind)
		}
		budget := record(next["consultationBudget"])
		if next["usageMeasurementHold"] != nil || budget["pendingCall"] != nil || budget["unknownCalls"] != float64(0) {
			t.Fatalf("%s: 保留または未確定の呼び出しが残りました %v", kind, next)
		}
		if budget["calls"] != float64(5) || budget["weighted"] != float64(100) {
			t.Fatalf("%s: 呼び出し回数と使用量を書き換えました %v", kind, budget)
		}
		releases := fleet.Items(budget["measurementReleases"])
		if len(releases) != 1 || record(releases[0])["kind"] != kind || record(releases[0])["diagnosticId"] != "diag-1" || record(releases[0])["messageId"] != "m1" {
			t.Fatalf("%s: 解除の記録がありません %v", kind, releases)
		}
	}
	for _, kind := range []string{"completed", "authentication", "usage_limit", "format", "spawn_failed", ""} {
		next := heldDraft(kind)
		if releaseUnmeasuredHold(next, "m1", "2026-11-03T01:00:00Z") || next["usageMeasurementHold"] == nil {
			t.Fatalf("%q の保留を解除しました", kind)
		}
	}
	for _, reason := range []string{"authentication", "usage_limit", "rate_limit", "input_invalid"} {
		next := heldDraft("exit_nonzero")
		record(record(next["usageMeasurementHold"])["diagnostic"])["failureReason"] = reason
		if releaseUnmeasuredHold(next, "m1", "x") || next["usageMeasurementHold"] == nil {
			t.Fatalf("failureReason %s の保留を解除しました", reason)
		}
	}
	for _, field := range []struct{ key, value string }{{"reason", "record_failed"}, {"originalFailure", "authentication"}, {"originalFailure", "usage_limit"}, {"originalFailure", "format"}} {
		next := heldDraft("exit_nonzero")
		record(next["usageMeasurementHold"])[field.key] = field.value
		if releaseUnmeasuredHold(next, "m1", "x") || next["usageMeasurementHold"] == nil {
			t.Fatalf("%s=%s の保留を解除しました", field.key, field.value)
		}
	}
	if releaseUnmeasuredHold(Record{}, "m1", "x") {
		t.Fatal("保留が無いのに解除しました")
	}
}

func TestReleaseUnmeasuredHoldStopsAfterTheLimit(t *testing.T) {
	for _, kind := range []string{"timeout_idle", "exit_nonzero"} {
		next := heldDraft(kind)
		for i := 0; i < maxMeasurementReleases; i++ {
			if !releaseUnmeasuredHold(next, "m", "2026-11-03T01:00:00Z") {
				t.Fatalf("%d回目を解除しません", i+1)
			}
			next["usageMeasurementHold"] = heldDraft(kind)["usageMeasurementHold"]
		}
		if releaseUnmeasuredHold(next, "m", "2026-11-03T01:00:00Z") || next["usageMeasurementHold"] == nil {
			t.Fatal("上限を超えて解除しました")
		}
	}
}

func holdFixtureDraft(t *testing.T, s *Service, kind string) {
	t.Helper()
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "hold": true}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		for k, v := range heldDraft(kind) {
			d[k] = v
		}
		d["status"], d["error"] = "error", "前のAI呼び出しの使用量またはその保存を確定できないため、自動の呼び出しを停止しました。"
		return d, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChatReplyReleasesTimeoutHoldButAutomaticRetryDoesNot(t *testing.T) {
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}

	for _, kind := range []string{"timeout_idle", "exit_nonzero"} {
		s, _, _ := fixture(t)
		holdFixtureDraft(t, s, kind)
		d, _ := s.Draft(ctx, todoID, p)
		if _, err := s.Chat(ctx, todoID, Record{"retry": true, "revision": d["revision"]}, p); err != nil {
			t.Fatal(err)
		}
		d, _ = s.Draft(ctx, todoID, p)
		if d["usageMeasurementHold"] == nil {
			t.Fatal("自動の再試行が保留を解除しました")
		}

		s, _, count := fixture(t)
		holdFixtureDraft(t, s, kind)
		if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "読む範囲を絞ります"}, p); err != nil {
			t.Fatal(err)
		}
		d, _ = s.Draft(ctx, todoID, p)
		budget := record(d["consultationBudget"])
		if d["usageMeasurementHold"] != nil || fmt.Sprint(budget["unknownCalls"]) != "0" || budget["pendingCall"] != nil || len(fleet.Items(budget["measurementReleases"])) != 1 || fmt.Sprint(budget["calls"]) != "5" || count.Load() != 1 {
			t.Fatal("利用者の返信で保留が解除されず、相談が再開されませんでした", d, count.Load())
		}

	}
	s, _, _ := fixture(t)
	holdFixtureDraft(t, s, "authentication")
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "返信"}, p); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Draft(ctx, todoID, p)
	if d["usageMeasurementHold"] == nil {
		t.Fatal("時間切れでない保留を解除しました")
	}
}

func TestReleaseInterruptedCallWithoutHoldOnlyWhenACallWasLeftPending(t *testing.T) {
	next := Record{"consultationBudget": Record{"calls": float64(3), "weighted": float64(10), "unknownCalls": float64(1), "pendingCall": Record{"id": "p1", "at": "2026-11-03T02:26:15Z"}}}
	if !releaseUnmeasuredHold(next, "m2", "2026-11-03T03:00:00Z") {
		t.Fatal("中断された呼び出しを解除しません")
	}
	budget := record(next["consultationBudget"])
	releases := fleet.Items(budget["measurementReleases"])
	if budget["pendingCall"] != nil || fmt.Sprint(budget["unknownCalls"]) != "0" || fmt.Sprint(budget["calls"]) != "3" || len(releases) != 1 {
		t.Fatal("未確定の印が残った、または呼び出し回数を書き換えました", budget)
	}
	if record(releases[0])["callId"] != "p1" || record(releases[0])["kind"] != "interrupted" || record(releases[0])["pendingAt"] != "2026-11-03T02:26:15Z" {
		t.Fatal("解除の記録が不足しています", releases[0])
	}
	// 完了の記録が無い呼び出しが無く、使用量が未確定の呼び出しの件数だけがあり、最後の診断が時間切れでない場合は解除しない。
	for _, diagnostics := range [][]any{nil, {Record{"kind": "authentication", "id": "d1"}}, {Record{"kind": "timeout_idle", "id": "d0"}, Record{"kind": "exit_nonzero", "id": "d1"}}} {
		counted := Record{"consultationBudget": Record{"calls": float64(3), "unknownCalls": float64(2)}, "aiDiagnostics": diagnostics}
		if CanReleaseMeasurementHold(counted) || releaseUnmeasuredHold(counted, "m3", "x") || fmt.Sprint(record(counted["consultationBudget"])["unknownCalls"]) != "2" {
			t.Fatal("使用量が未確定の呼び出しを解除しました", diagnostics)
		}
	}
}

// 下書き 34445aea: 時間切れの後の1回だけの再試行も時間切れで終わり、保留も途中の呼び出しも無く
// unknownCalls=1 だけが残った。返信で解除して再開し、解除の記録を残し、上限3回は変わらない。
func TestReplyReleasesUnknownCallLeftByTimedOutRetry(t *testing.T) {
	next := Record{
		"consultationBudget": Record{"calls": float64(13), "weighted": float64(500), "unknownCalls": float64(1)},
		"aiDiagnostics":      []any{Record{"kind": "timeout_idle", "id": "dc285345fade7973", "stage": "exit"}},
	}
	if !CanReleaseMeasurementHold(next) || !releaseUnmeasuredHold(next, "m1", "2026-11-17T04:32:00Z") {
		t.Fatal("時間切れで残った未確定の呼び出しを解除しません")
	}
	budget := record(next["consultationBudget"])
	releases := fleet.Items(budget["measurementReleases"])
	if fmt.Sprint(budget["unknownCalls"]) != "0" || fmt.Sprint(budget["calls"]) != "13" || fmt.Sprint(budget["weighted"]) != "500" || len(releases) != 1 {
		t.Fatal("解除で使用量や呼び出し回数を書き換えた、または記録が無い", budget)
	}
	if r := record(releases[0]); r["kind"] != "timeout_idle" || r["diagnosticId"] != "dc285345fade7973" || r["messageId"] != "m1" {
		t.Fatal("解除の記録が不足しています", releases[0])
	}
	budget["unknownCalls"] = float64(1)
	budget["measurementReleases"] = []any{Record{}, Record{}, Record{}}
	if CanReleaseMeasurementHold(next) || releaseUnmeasuredHold(next, "m4", "x") {
		t.Fatal("上限の3回を超えて解除しました")
	}
}

func TestMeasurementRetryActionsMatchScreenProjection(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	cases := []struct {
		id        string
		fields    Record
		wantRetry bool
	}{
		{"timeout", Record{"usageMeasurementHold": Record{"diagnostic": Record{"kind": "timeout_idle"}}}, false},
		{"other-hold", Record{"usageMeasurementHold": Record{"diagnostic": Record{"kind": "authentication"}}}, false},
		{"old-hold", Record{"usageMeasurementHold": Record{}}, false},
		{"pending", Record{"consultationBudget": Record{"pendingCall": Record{"id": "private"}}}, false},
		{"unknown", Record{"consultationBudget": Record{"unknownCalls": 2}}, false},
		{"clear", Record{"consultationBudget": Record{"unknownCalls": 0, "pendingCall": nil}}, true},
		{"missing", Record{}, true},
	}
	for _, tc := range cases {
		d := Record{"id": tc.id, "project": "owner/app", "status": "error", "messages": []any{Record{"role": "user", "text": "依頼"}}}
		for k, v := range tc.fields {
			d[k] = v
		}
		if _, err := s.options.Records.Change(ctx, "drafts", tc.id, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.options.Records.ListDraftEntries(ctx, DraftReadScreen)
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	views := s.DraftScreenViews(entries, p, "")
	byID := make(map[string]Record)
	for i, entry := range entries {
		byID[entry.Key] = views[i]
		budget := record(entry.Record["consultationBudget"])
		if record(budget["pendingCall"]) != nil || record(entry.Record["usageMeasurementHold"]) != nil {
			t.Fatal("projection contains hold or pending details")
		}
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			full, _, err := s.options.Records.Get(ctx, "drafts", tc.id)
			if err != nil {
				t.Fatal(err)
			}
			fullActions := s.DraftView(full, p, "")["actions"].([]string)
			screenActions := byID[tc.id]["actions"].([]string)
			if !reflect.DeepEqual(fullActions, screenActions) {
				t.Fatalf("full=%v screen=%v", fullActions, screenActions)
			}
			if slices.Contains(fullActions, "retry") != tc.wantRetry {
				t.Fatalf("actions=%v", fullActions)
			}
			if reason := draftRefusal(full, "retry", false); reason != "" {
				t.Fatalf("direct retry changed: %s", reason)
			}
		})
	}
}
