package appsql

import (
	"errors"
	"reflect"
	"testing"
)

type closeFailRows struct {
	*FakeRows
	err error
}

func (r closeFailRows) Close() error { r.FakeRows.Close(); return r.err }

func TestJSONObjectsFailsOnInvalidRow(t *testing.T) {
	rows := &FakeRows{Values: [][]any{{`{"a":1}`}, {`{broken`}, {`{"b":2}`}}}
	out, skipped, err := JSONObjects[string](rows, JSONRowsOptions{})
	if err == nil || out != nil || skipped != 0 {
		t.Fatalf("got %v %d %v, want nil 0 error", out, skipped, err)
	}
	if !rows.Closed {
		t.Fatal("rows not closed")
	}
}

func TestJSONObjectsSkipsInvalidRowsInOrder(t *testing.T) {
	rows := &FakeRows{Values: [][]any{{`{"a":1}`}, {`{broken`}, {`null`}, {`[1]`}, {`{"b":2}`}}}
	out, skipped, err := JSONObjects[string](rows, JSONRowsOptions{SkipInvalid: true})
	if err != nil || skipped != 2 || !rows.Closed {
		t.Fatalf("got skipped=%d err=%v closed=%v", skipped, err, rows.Closed)
	}
	want := []map[string]any{{"a": 1.0}, nil, {"b": 2.0}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %#v, want %#v", out, want)
	}
}

func TestJSONObjectsNoRowsIsEmptyNotNil(t *testing.T) {
	out, skipped, err := JSONObjects[string](&FakeRows{}, JSONRowsOptions{})
	if err != nil || out == nil || len(out) != 0 || skipped != 0 {
		t.Fatalf("got %#v %d %v, want empty non-nil", out, skipped, err)
	}
}

func TestJSONObjectsScanErrorFailsEvenWhenSkipping(t *testing.T) {
	rows := &FakeRows{Values: [][]any{{`{"a":1}`}, {7}}}
	out, skipped, err := JSONObjects[string](rows, JSONRowsOptions{SkipInvalid: true})
	if err == nil || out != nil || skipped != 0 || !rows.Closed {
		t.Fatalf("got %v %d %v closed=%v, want nil 0 error closed", out, skipped, err, rows.Closed)
	}
}

func TestJSONObjectsRowsErrKeepsRowsRead(t *testing.T) {
	failure := errors.New("iteration")
	rows := &FakeRows{Values: [][]any{{`{"a":1}`}, {`{broken`}}, Error: failure}
	out, skipped, err := JSONObjects[string](rows, JSONRowsOptions{SkipInvalid: true})
	if !errors.Is(err, failure) || skipped != 1 || !reflect.DeepEqual(out, []map[string]any{{"a": 1.0}}) {
		t.Fatalf("got %#v %d %v", out, skipped, err)
	}
}

func TestJSONObjectsReturnsCloseError(t *testing.T) {
	failure := errors.New("close")
	out, _, err := JSONObjects[string](closeFailRows{&FakeRows{Values: [][]any{{`{"a":1}`}}}, failure}, JSONRowsOptions{})
	if !errors.Is(err, failure) || len(out) != 1 {
		t.Fatalf("got %#v %v, want rows and close error", out, err)
	}
	scanFailure := closeFailRows{&FakeRows{Values: [][]any{{7}}}, failure}
	if _, _, err = JSONObjects[string](scanFailure, JSONRowsOptions{}); err == nil || errors.Is(err, failure) {
		t.Fatalf("scan error must win over close error, got %v", err)
	}
}

func TestJSONObjectsByteColumnNull(t *testing.T) {
	rows := &FakeRows{Values: [][]any{{nil}, {[]byte(`{"a":1}`)}}}
	out, skipped, err := JSONObjects[[]byte](rows, JSONRowsOptions{SkipInvalid: true})
	if err != nil || skipped != 1 || !reflect.DeepEqual(out, []map[string]any{{"a": 1.0}}) {
		t.Fatalf("got %#v %d %v", out, skipped, err)
	}
	if _, _, err = JSONObjects[[]byte](&FakeRows{Values: [][]any{{nil}}}, JSONRowsOptions{}); err == nil {
		t.Fatal("NULL column must fail without SkipInvalid")
	}
}

func TestJSONObjectsByID(t *testing.T) {
	sizes := []int{}
	rows := &FakeRows{Values: [][]any{{"x", `{"a":1}`}, {"y", `null`}, {"x", `{"a":2}`}}}
	out, skipped, err := JSONObjectsByID[string](rows, JSONRowsOptions{Observe: func(n int) { sizes = append(sizes, n) }})
	if err != nil || skipped != 0 || !rows.Closed {
		t.Fatalf("got skipped=%d err=%v closed=%v", skipped, err, rows.Closed)
	}
	want := map[string]map[string]any{"x": {"a": 2.0}, "y": nil}
	if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(sizes, []int{8, 5, 8}) {
		t.Fatalf("got %#v sizes=%v", out, sizes)
	}
}

func TestJSONObjectsByIDFailsAfterObserving(t *testing.T) {
	sizes := []int{}
	rows := &FakeRows{Values: [][]any{{"x", `{broken`}}}
	out, _, err := JSONObjectsByID[string](rows, JSONRowsOptions{Observe: func(n int) { sizes = append(sizes, n) }})
	if err == nil || out != nil || !reflect.DeepEqual(sizes, []int{8}) {
		t.Fatalf("got %#v %v sizes=%v", out, err, sizes)
	}
}
