package consultations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

// tierRequired reports whether err is the refusal for a consultation the
// requester started without choosing its tier.
func tierRequired(err error) bool {
	var problem *contract.Problem
	return errors.As(err, &problem) && problem.Status == 400 && problem.Code == "TIER_REQUIRED" && strings.Contains(problem.Error(), "--tier")
}

// The requester always chooses the consultation tier: every entry that starts a
// consultation refuses a request without it and saves nothing.
func TestNewConsultationRequiresTier(t *testing.T) {
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	for _, tier := range []any{nil, ""} {
		s, _, _ := fixture(t)
		input := Record{"id": todoID, "project": "owner/app", "text": "本文"}
		if tier != nil {
			input["tier"] = tier
		}
		if _, err := s.CreateDraft(ctx, input, p, false); !tierRequired(err) {
			t.Fatalf("tier=%v の下書きを断らない: %v", tier, err)
		}
		if _, err := s.AcceptIssue(ctx, input, p); !tierRequired(err) {
			t.Fatalf("tier=%v の依頼を断らない: %v", tier, err)
		}
		if _, found, err := s.options.Records.Get(ctx, "drafts", todoID); err != nil || found {
			t.Fatalf("断った依頼の下書きが残る: %v %v", found, err)
		}
		var receipts int
		if err := s.options.Records.DB.QueryRow("SELECT COUNT(*) FROM intake_receipts").Scan(&receipts); err != nil || receipts != 0 {
			t.Fatalf("断った依頼の受領が残る: %d %v", receipts, err)
		}
	}
	s, _, _ := fixture(t)
	if _, err := s.CreateDraft(ctx, Record{"id": todoID, "project": "owner/app", "tier": "maximum"}, p, false); err == nil || tierRequired(err) {
		t.Fatalf("不正な難易度の理由が違う: %v", err)
	}
	d, err := s.CreateDraft(ctx, Record{"id": todoID, "project": "owner/app", "tier": "heavy"}, p, false)
	if err != nil || d["requestedTier"] != "heavy" || record(d["tier"])["pending"] != nil {
		t.Fatalf("指定した難易度で始まらない: %v %v", d, err)
	}
}

// Starting a TODO is also a new consultation: without a tier it is refused and
// the TODO stays; with one the draft keeps the requester's choice.
func TestTodoStartRequiresTier(t *testing.T) {
	s, _, count := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": "相談"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartTodo(ctx, todoID, Record{"revision": 1}, p); !tierRequired(err) {
		t.Fatalf("tier無しの開始を断らない: %v", err)
	}
	if _, found, err := s.options.Records.Get(ctx, "todos", todoID); err != nil || !found || count.Load() != 0 {
		t.Fatalf("断った開始でTODOが消えた・AIが動いた: %v %v %d", found, err, count.Load())
	}
	draft, err := s.StartTodo(ctx, todoID, Record{"revision": 1, "tier": "standard"}, p)
	if err != nil || draft["requestedTier"] != "standard" {
		t.Fatalf("指定した難易度で始まらない: %v %v", draft, err)
	}
}

// The HTTP entries answer 400 with code TIER_REQUIRED and a message that tells
// an old create-issue.ts to update, so the cause is visible to the requester.
func TestTierRequiredHTTP(t *testing.T) {
	for _, path := range []string{"/api/drafts", "/api/issues"} {
		t.Run(path, func(t *testing.T) {
			s, db, _ := fixture(t)
			key := "tier-required-key"
			if _, err := db.DB().Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash(key), time.Now().Add(time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			s.Register(mux)
			req := httptest.NewRequest("POST", "http://localhost:8787"+path, strings.NewReader(`{"id":"`+todoID+`","project":"owner/app","text":"本文"}`))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Origin", "http://localhost:8787")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Desk-Key", key)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			body := w.Body.String()
			if w.Code != 400 || !strings.Contains(body, "TIER_REQUIRED") || !strings.Contains(body, "create-issue.ts") {
				t.Fatalf("status=%d body=%s", w.Code, body)
			}
		})
	}
}

// A receipt accepted before the tier became required has no tier. Recovering it
// after the update must not refuse the already accepted request: it starts at
// heavy (the level the removed classifier started at) and says why, and a
// second recovery of the same receipt returns the same draft.
func TestLegacyReceiptWithoutTierStartsHeavy(t *testing.T) {
	s, _, _ := fixture(t)
	s.options.InitialTier = func(_ string, requested string, at time.Time) (Record, error) {
		return Record{"level": requested, "source": "user", "reason": "依頼者の指定", "decidedAt": timestamp(at), "history": []any{}}, nil
	}
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	input := Record{"id": todoID, "project": "owner/app", "text": "本文", "hold": true}
	for i := 0; i < 2; i++ {
		d, err := s.createReceiptDraft(ctx, input, p)
		if err != nil {
			t.Fatalf("%d回目: %v", i+1, err)
		}
		tier := record(d["tier"])
		if tier["level"] != "heavy" || tier["source"] != "default" || tier["reason"] != legacyReceiptTierReason {
			t.Fatalf("%d回目の難易度: %v", i+1, tier)
		}
	}
	if _, exists := input["tier"]; exists {
		t.Fatal("受領の入力を書き換えた")
	}
	if _, err := s.createReceiptDraft(ctx, Record{"id": sendID, "project": "owner/app", "text": "本文", "tier": "light"}, p); err != nil {
		t.Fatal(err)
	}
	d, _, _ := s.options.Records.Get(ctx, "drafts", sendID)
	if tier := record(d["tier"]); tier["level"] != "light" || tier["source"] != "user" {
		t.Fatalf("指定のある受領の難易度: %v", tier)
	}
}
