package notices

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/pgtest"
	"testing"
	"time"
)

func obj(raw string) map[string]any {
	var v map[string]any
	_ = json.Unmarshal([]byte(raw), &v)
	return v
}
func fixture(t *testing.T) *Service {
	t.Helper()
	return &Service{DB: pgtest.New(t), Now: func() time.Time { return time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC) }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
}
func TestOpsReceiptSurvivesAcknowledgmentAndMergesCause(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	first := obj(`{"kind":"ops-watch","id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","text":"check","reason":"failed","issues":[],"cause":"network","targets":[{"repo":"owner/repo","number":1}]}`)
	result, err := s.Receive(ctx, "nas", first)
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["id"].(string)
	second := obj(`{"kind":"ops-watch","id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","text":"check 2","reason":"failed","issues":[],"cause":"network","targets":[{"repo":"owner/repo","number":2}]}`)
	result, err = s.Receive(ctx, "nas", second)
	if err != nil || result.(map[string]any)["id"] != id {
		t.Fatalf("merge %v %v", result, err)
	}
	if _, err = s.Acknowledge(ctx, id); err != nil {
		t.Fatal(err)
	}
	result, err = s.Receive(ctx, "nas", first)
	if err != nil || result.(map[string]any)["id"] != id {
		t.Fatalf("receipt %v %v", result, err)
	}
	first["reason"] = "changed"
	if _, err = s.Receive(ctx, "nas", first); err == nil {
		t.Fatal("conflicting ID accepted")
	}
	var raw string
	if err = s.DB.QueryRow("SELECT json FROM settings WHERE id='ops-watch-notices'").Scan(&raw); err != nil || raw != "[]" {
		t.Fatalf("notice resurrected %s %v", raw, err)
	}
}
func TestCheckoutCannotRegressAndAppHealthDeduplicates(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	body := obj(`{"kind":"desk-checkout-lock","observedAt":"2026-11-01T00:00:00Z","blockedSince":"2026-10-31T23:00:00Z"}`)
	if _, err := s.Receive(ctx, "nas", body); err != nil {
		t.Fatal(err)
	}
	body["blockedSince"] = nil
	if _, err := s.Receive(ctx, "nas", body); err == nil {
		t.Fatal("conflicting same observation accepted")
	}
	health := obj(`{"kind":"app-health","repo":"owner/repo","category":"store-failed","count":2,"at":"2026-11-01T00:00:00Z","intervalMinutes":5}`)
	if _, err := s.Receive(ctx, "nas", health); err != nil {
		t.Fatal(err)
	}
	value, err := s.Receive(ctx, "nas", health)
	if err != nil || value.(map[string]any)["duplicate"] != true {
		t.Fatalf("duplicate %v %v", value, err)
	}
	health["repo"] = "other/repo"
	if _, err = s.Receive(ctx, "nas", health); err == nil {
		t.Fatal("unregistered repository accepted")
	}
}
