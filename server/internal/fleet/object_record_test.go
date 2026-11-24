package fleet

import "testing"

type namedObject map[string]any

func TestObjectAcceptsNamedMaps(t *testing.T) {
	original := namedObject{"number": 42}
	got := Object(original)
	if got["number"] != 42 {
		t.Fatalf("named map: %#v", got)
	}
	got["number"] = 43
	if original["number"] != 43 {
		t.Fatal("conversion must preserve map identity")
	}
	if Object(nil) != nil || Object(map[string]string{"key": "value"}) != nil {
		t.Fatal("unrelated values must not become objects")
	}
}
