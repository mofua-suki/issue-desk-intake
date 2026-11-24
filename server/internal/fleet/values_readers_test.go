package fleet

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestObjectOrEmptyIsWritable(t *testing.T) {
	original := namedObject{"number": 42}
	if got := ObjectOrEmpty(original); got["number"] != 42 {
		t.Fatalf("named map: %#v", got)
	}
	for _, value := range []any{nil, "text", map[string]any(nil), []any{}} {
		got := ObjectOrEmpty(value)
		if got == nil || len(got) != 0 {
			t.Fatalf("%#v gave %#v", value, got)
		}
		got["written"] = true
	}
}

func TestElementsKeepsTypedElements(t *testing.T) {
	records := []namedObject{{"id": "a"}, {"id": "b"}}
	got := Elements(records)
	if len(got) != 2 || !reflect.DeepEqual(got[1], namedObject{"id": "b"}) {
		t.Fatalf("typed slice: %#v", got)
	}
	if got := Elements([2]int{1, 2}); !reflect.DeepEqual(got, []any{1, 2}) {
		t.Fatalf("array: %#v", got)
	}
	if Elements(nil) != nil || Elements("text") != nil || Elements(map[string]any{}) != nil {
		t.Fatal("non-lists must give nil")
	}
}

func TestItemsReadsTheJSONForm(t *testing.T) {
	got := Items([]namedObject{{"n": 1}})
	if !reflect.DeepEqual(got, []any{map[string]any{"n": float64(1)}}) {
		t.Fatalf("typed slice: %#v", got)
	}
	same := []any{"x"}
	if got := Items(same); &got[0] != &same[0] {
		t.Fatal("[]any must be returned as is")
	}
	if Items(nil) != nil || Items("text") != nil || Items(map[string]any{}) != nil {
		t.Fatal("non-lists must give nil")
	}
}

func TestIntegerReaders(t *testing.T) {
	for _, test := range []struct {
		value        any
		int64, whole int64
	}{
		{3, 3, 3}, {int64(1) << 60, 1 << 60, 1 << 60}, {int32(7), 0, 7},
		{2.0, 2, 2}, {2.7, 2, 0}, {json.Number("12"), 12, 12}, {json.Number("1.5"), 0, 0},
		{"5", 0, 0}, {nil, 0, 0},
	} {
		if got := Int64(test.value); got != test.int64 {
			t.Errorf("Int64(%#v)=%d, want %d", test.value, got, test.int64)
		}
		if got := WholeNumber(test.value); got != test.whole {
			t.Errorf("WholeNumber(%#v)=%d, want %d", test.value, got, test.whole)
		}
	}
}
