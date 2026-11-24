package filing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPrepareRejectsDeterministicErrorsBeforeCurrentCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(Record)
	}{
		{"body limit", func(d Record) { d["body"] = strings.Repeat("あ", 20001) }},
		{"execution format", func(d Record) { d["executionPlan"] = Record{"version": 999} }},
		{"unreviewed image", func(d Record) { d["messages"] = []any{Record{"role": "user", "images": []any{Record{"id": "image"}}}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := draftFixture()
			tc.change(d)
			calls := 0
			c := configFixture()
			c.CheckCurrent = func(context.Context, Record) error { calls++; return nil }
			if _, err := Prepare(context.Background(), c, d); err == nil {
				t.Fatal("invalid input accepted")
			}
			if calls != 0 {
				t.Fatalf("current-state check called %d times for invalid input", calls)
			}
		})
	}
	d := draftFixture()
	c := configFixture()
	calls := 0
	blocked := errors.New("comparison rejected")
	c.CheckCurrent = func(context.Context, Record) error { calls++; return blocked }
	if _, err := Prepare(context.Background(), c, d); !errors.Is(err, blocked) || calls != 1 {
		t.Fatalf("current check bypassed: calls=%d err=%v", calls, err)
	}
	c.CheckCurrent = func(context.Context, Record) error { calls++; return nil }
	if _, err := Prepare(context.Background(), c, d); err != nil || calls != 2 {
		t.Fatalf("valid filing: calls=%d err=%v", calls, err)
	}
}
