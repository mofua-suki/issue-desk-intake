package operations

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/pgtest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) *Service {
	t.Helper()
	return &Service{DB: pgtest.New(t), RuntimeDir: t.TempDir(), Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}
}
func TestLeaseOwnershipExpiryAndPrimary(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	body := map[string]any{"ttlMinutes": float64(1), "primary": false}
	for _, step := range []struct {
		host          string
		primary, held bool
	}{{"a", false, true}, {"b", false, false}, {"a", false, true}, {"b", true, true}} {
		body["primary"] = step.primary
		r, e := s.Lease(ctx, step.host, body)
		if e != nil || r["held"] != step.held {
			t.Fatalf("%+v: %v %v", step, r, e)
		}
	}
	s.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC) }
	body["primary"] = false
	r, e := s.Lease(ctx, "a", body)
	if e != nil || r["held"] != true {
		t.Fatalf("expiry %v %v", r, e)
	}
	body["ttlMinutes"] = 1.5
	if _, e = s.Lease(ctx, "a", body); e == nil {
		t.Fatal("fraction accepted")
	}
}
func TestForceHandoffAuditBeforePublication(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	target := filepath.Join(s.RuntimeDir, "state", "intake-force-handoff.request")
	body := map[string]any{"statement": "停止中のNASの処理を今入れ替えてください"}
	// 利用者が指示した日時の無い・未来の・7日より前の要求は、監査も保存もせずに断る。
	s.Audit = func(context.Context, string, map[string]any) error {
		t.Fatal("audited without instructionAt")
		return nil
	}
	for _, at := range []any{nil, "", "2026-11-01T09:00:00", "2026-11-01T09:01:00+09:00", "2026-10-25T08:59:00+09:00"} {
		if at != nil {
			body["instructionAt"] = at
		}
		if _, e := s.ForceHandoff(ctx, "nas", body); e == nil {
			t.Fatalf("instructionAt %v accepted", at)
		}
	}
	body["instructionAt"] = "2026-11-01T08:58:00+09:00"
	s.Audit = func(context.Context, string, map[string]any) error { return errors.New("disk failure") }
	if _, e := s.ForceHandoff(ctx, "nas", body); e == nil {
		t.Fatal("audit failure ignored")
	}
	if _, e := os.Stat(target); !os.IsNotExist(e) {
		t.Fatalf("request published: %v", e)
	}
	audited := ""
	s.Audit = func(_ context.Context, id string, value map[string]any) error {
		if _, e := os.Stat(target); !os.IsNotExist(e) {
			t.Fatal("published before audit")
		}
		audited = id
		if value["statement"] != body["statement"] || value["instructionAt"] != body["instructionAt"] {
			t.Fatal("statement or instructionAt lost")
		}
		return nil
	}
	r, e := s.ForceHandoff(ctx, "nas", body)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(target)
	if e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 6 || lines[0] != "v1" || lines[3] != "*" || lines[4] != audited || r["id"] != audited {
		t.Fatalf("request %s %v", raw, r)
	}
	if stat, _ := os.Stat(target); stat.Mode().Perm() != 0600 {
		t.Fatal("permissions")
	}
}
func TestReviewAcknowledgmentIsDurable(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	var e error
	if _, e = s.AcknowledgeReview(ctx, 1); e == nil {
		t.Fatal("missing round acknowledged")
	}
	if _, e = s.DB.Exec("INSERT INTO usage_review_rounds(round,json) VALUES(1,'{}')"); e != nil {
		t.Fatal(e)
	}
	first, e := s.AcknowledgeReview(ctx, 1)
	if e != nil {
		t.Fatal(e)
	}
	s.Now = func() time.Time { return time.Now() }
	second, e := s.AcknowledgeReview(ctx, 1)
	if e != nil || first.(map[string]any)["acknowledgedAt"] != second.(map[string]any)["acknowledgedAt"] {
		t.Fatal("ack timestamp changed")
	}
}
