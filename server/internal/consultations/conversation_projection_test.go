package consultations

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func TestConversationProjectionPreservesRecords(t *testing.T) {
	ctx := context.Background()
	s, _, _ := fixture(t)
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	messages := []any{
		Record{"id": "first", "role": "user", "text": "元の発言😀", "at": "2026-11-01T03:00:00Z", "source": "screen-session", "references": []any{Record{"source": "issue", "version": "old", "target": "owner/app#1", "body": strings.Repeat("旧本文", 400), "state": Record{"status": "closed"}, "history": "後続判断 投稿者 alice"}}},
		Record{"id": "second", "role": "assistant", "text": "回答", "at": "2026-11-01T03:01:00Z"},
	}
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["messages"] = messages; return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	original := fleet.Items(before["messages"])
	projection, err := projectConversation(original, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Omitted) != 1 {
		t.Fatalf("omitted: %+v", projection.Omitted)
	}
	after, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, fleet.Items(after["messages"])) {
		t.Fatalf("stored messages changed: %v", after["messages"])
	}
	for i, raw := range projection.Messages {
		got, want := record(raw), record(original[i])
		for _, key := range []string{"id", "role", "text", "at", "source"} {
			if !reflect.DeepEqual(got[key], want[key]) {
				t.Fatalf("message %d %s changed", i, key)
			}
		}
	}
}

func TestConversationProjectionSelection(t *testing.T) {
	refs := []any{Record{"source": "issue", "version": "v1", "target": "a#1", "body": strings.Repeat("😀", 500)}, Record{"source": "issue", "version": "v2", "target": "a#2", "body": "最新本文"}}
	messages := []any{Record{"id": "1", "text": "目的・受入条件", "references": refs}}
	full, err := projectConversation(messages, 80000)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Omitted) != 0 || !reflect.DeepEqual(record(full.Messages[0])["references"], refs) {
		t.Fatalf("full: %+v", full)
	}
	selected, err := projectConversation(messages, 500)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := canonicalJSON(selected)
	if err != nil {
		t.Fatal(err)
	}
	if contract.UTF16Length(encoded) > 500 || len(selected.Omitted) != 1 || !reflect.DeepEqual(selected.Omitted[0], OmittedConversationReference{MessageIndex: 0, ReferenceIndex: 0, Unconfirmed: true, Source: map[string]any{"source": "issue", "version": "v1", "target": "a#1", "incomplete": false}}) {
		t.Fatalf("selection: %+v", selected)
	}
	got := fleet.Items(record(selected.Messages[0])["references"])
	if len(got) != 1 || !reflect.DeepEqual(got[0], refs[1]) {
		t.Fatalf("reference identity lost: %v", got)
	}
	failed, err := projectConversation(messages, 10)
	var short *ProjectionShortfall
	if !errors.As(err, &short) || short.Shortfall != short.Required-10 || short.Messages <= 0 || short.Omitted <= 0 || len(failed.Omitted) != 1 {
		t.Fatalf("shortfall: %+v %v", failed, err)
	}
	if !reflect.DeepEqual(record(messages[0])["references"], refs) {
		t.Fatal("source modified")
	}
}

func TestConversationProjectionInactive(t *testing.T) {
	ctx := context.Background()
	s, _, _ := fixture(t)
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	original := []any{Record{"id": "original", "role": "user", "text": "元の発言", "references": []any{Record{"source": "issue", "body": strings.Repeat("参照", 1000)}}}}
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["messages"] = original; return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	var input Record
	s.options.Start = func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) {
		input = d
		return d, nil
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "補足"}, p); err != nil {
		t.Fatal(err)
	}
	sent := fleet.Items(input["messages"])
	expected, _ := canonicalJSON(record(original[0])["references"])
	actual, _ := canonicalJSON(record(sent[0])["references"])
	if len(sent) != 2 || text(record(sent[1])["text"]) != "補足" || actual != expected {
		t.Fatal("AI input changed")
	}
}

func TestConversationProjectionOmittedSourceIdentifiers(t *testing.T) {
	refs := []any{
		Record{"desk": 6131, "project": "owner/repo", "number": 2218, "source": "issue", "version": "v1", "target": "owner/repo#2218", "body": strings.Repeat("secret", 1000), "history": "private", "title": "title", "unknown": Record{"nested": "private"}},
		Record{"project": "owner/repo", "gitea": 5, "body": strings.Repeat("other", 1000)},
	}
	p, err := projectConversation([]any{Record{"references": refs}}, 600)
	if err != nil || len(p.Omitted) != 2 {
		t.Fatalf("projection: %+v %v", p, err)
	}
	want := []map[string]any{{"desk": 6131, "project": "owner/repo", "number": 2218, "source": "issue", "version": "v1", "target": "owner/repo#2218", "incomplete": false}, {"project": "owner/repo", "gitea": 5, "incomplete": false}}
	for i, o := range p.Omitted {
		if !reflect.DeepEqual(o.Source, want[i]) || o.MessageIndex != 0 || o.ReferenceIndex != i || !o.Unconfirmed {
			t.Fatalf("source %d: %+v", i, o)
		}
	}
}

func TestConversationProjectionOmittedSourceLimits(t *testing.T) {
	refs := []any{Record{"desk": 42, "project": strings.Repeat("😀", 600), "version": Record{"nested": "secret"}, "body": strings.Repeat("x", 3000)}, Record{"target": "", "number": []any{1}, "body": strings.Repeat("y", 3000)}}
	before, _ := canonicalJSON(refs)
	p, err := projectConversation([]any{Record{"references": refs}}, 700)
	if err != nil || len(p.Omitted) != 2 {
		t.Fatalf("projection: %+v %v", p, err)
	}
	for _, o := range p.Omitted {
		encoded, err := canonicalJSON(o.Source)
		if err != nil || contract.UTF16Length(encoded) > 512 {
			t.Fatalf("source size: %s %v", encoded, err)
		}
	}
	if !reflect.DeepEqual(p.Omitted[0].Source["omittedFields"], []string{"project", "version"}) || p.Omitted[0].Source["incomplete"] != false || p.Omitted[1].Source["incomplete"] != true || !reflect.DeepEqual(p.Omitted[1].Source["omittedFields"], []string{"number", "target"}) {
		t.Fatalf("source flags: %+v", p.Omitted)
	}
	after, _ := canonicalJSON(refs)
	if before != after {
		t.Fatal("original changed")
	}
}

func TestConversationProjectionSourceBudgetBoundary(t *testing.T) {
	messages := []any{Record{"text": strings.Repeat("😀", 39800), "references": []any{Record{"desk": 1, "project": strings.Repeat("p", 300), "body": strings.Repeat("b", 1000)}}}}
	p, err := projectConversation(messages, 80000)
	encoded, e := canonicalJSON(p)
	if e != nil {
		t.Fatal(e)
	}
	// The same projected messages with position-only omissions fit; source metadata crosses the boundary.
	positionOnly := Record{"messages": p.Messages, "omitted": []any{Record{"messageIndex": 0, "referenceIndex": 0, "unconfirmed": true}}}
	baseline, e := canonicalJSON(positionOnly)
	if e != nil || contract.UTF16Length(baseline) > 80000 || err == nil {
		t.Fatalf("boundary not exercised: baseline=%d err=%v", contract.UTF16Length(baseline), err)
	}
	encodedMessages, e := canonicalJSON(p.Messages)
	if e != nil {
		t.Fatal(e)
	}
	encodedOmitted, e := canonicalJSON(p.Omitted)
	if e != nil {
		t.Fatal(e)
	}
	var short *ProjectionShortfall
	if !errors.As(err, &short) || short.Limit != 80000 || short.Required != contract.UTF16Length(encoded) || short.Shortfall != short.Required-short.Limit || short.Shortfall <= 0 || short.Messages != contract.UTF16Length(encodedMessages) || short.Omitted != contract.UTF16Length(encodedOmitted) || len(p.Omitted) != 1 {
		t.Fatalf("shortfall: %+v %v", short, err)
	}
	// Shorten only the message text by the measured excess, reaching the limit exactly.
	message := record(messages[0])
	message["text"] = strings.Repeat("x", 79600-short.Shortfall)
	fitting, err := projectConversation(messages, 80000)
	if err != nil {
		t.Fatalf("boundary should fit: %v", err)
	}
	encoded, err = canonicalJSON(fitting)
	if err != nil {
		t.Fatal(err)
	}
	if size := contract.UTF16Length(encoded); size != 80000 || len(fitting.Omitted) != 1 {
		t.Fatalf("fitting projection: size=%d omitted=%+v", size, fitting.Omitted)
	}
}
