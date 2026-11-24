package timedcache

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func copyList(v []any) []any {
	out, _ := CopyValue(v).([]any)
	return out
}

func TestCacheKeepsAValueUntilItsTimeEnds(t *testing.T) {
	c := New(5*time.Minute, copyList)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := c.Get("a", start); ok {
		t.Fatal("a value was found before any was put")
	}
	c.Put("a", []any{"first"}, start)
	if got, ok := c.Get("a", start.Add(5*time.Minute-time.Nanosecond)); !ok || !reflect.DeepEqual(got, []any{"first"}) {
		t.Fatalf("the value was not kept until the end of its time: %v %v", got, ok)
	}
	if _, ok := c.Get("b", start); ok {
		t.Fatal("a value was returned for a key that was never put")
	}
	if _, ok := c.Get("a", start.Add(5*time.Minute)); ok {
		t.Fatal("the value was still returned when its time had ended")
	}
	// A clock set back must not keep a value past its time.
	if _, ok := c.Get("a", start.Add(-time.Second)); ok {
		t.Fatal("the value was returned for a time before it was put")
	}
	c.Put("a", []any{"second"}, start.Add(6*time.Minute))
	if got, _ := c.Get("a", start.Add(7*time.Minute)); !reflect.DeepEqual(got, []any{"second"}) {
		t.Fatalf("a later put did not replace the value: %v", got)
	}
	// An empty key means the caller could not name its inputs.
	c.Put("", []any{"unnamed"}, start)
	if _, ok := c.Get("", start); ok {
		t.Fatal("a value was kept without a key")
	}
}

func TestCacheIsNotChangedThroughItsValues(t *testing.T) {
	c := New(time.Minute, copyList)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	put := []any{map[string]any{"title": "kept", "period": map[string]any{"from": "a"}, "tags": []any{"x"}}}
	c.Put("k", put, now)
	put[0].(map[string]any)["title"] = "changed after put"
	first, _ := c.Get("k", now)
	row := first[0].(map[string]any)
	row["title"] = "changed after get"
	row["period"].(map[string]any)["from"] = "b"
	row["tags"].([]any)[0] = "y"
	want := []any{map[string]any{"title": "kept", "period": map[string]any{"from": "a"}, "tags": []any{"x"}}}
	if got, _ := c.Get("k", now); !reflect.DeepEqual(got, want) {
		t.Fatalf("the kept value changed: %v", got)
	}
}

func TestCopyValueKeepsTypes(t *testing.T) {
	value := map[string]any{"count": 3, "rate": 0.5, "none": nil, "list": []any{1, "a", map[string]any{"n": int64(2)}}}
	if got := CopyValue(value); !reflect.DeepEqual(got, value) {
		t.Fatalf("copy differs: %#v", got)
	}
}

func TestNilCacheKeepsNothing(t *testing.T) {
	var c *Cache[[]any]
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	done, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Put("k", []any{"v"}, now)
	if _, ok := c.Get("k", now); ok {
		t.Fatal("a nil cache returned a value")
	}
	done()
}

func TestBeginLetsOneCallerLoadAtATime(t *testing.T) {
	c := New(time.Minute, copyList)
	done, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A waiting caller whose request ended must not wait for the load.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Begin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller did not stop waiting: %v", err)
	}
	entered := make(chan struct{})
	go func() {
		next, err := c.Begin(context.Background())
		if err == nil {
			next()
		}
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("a second caller began while the first was loading")
	case <-time.After(20 * time.Millisecond):
	}
	done()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the next caller did not begin after the first ended")
	}
}
