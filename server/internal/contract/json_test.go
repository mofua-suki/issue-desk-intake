package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeJobBodyBoundary(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType string
		limit                   int64
		valid                   bool
	}{
		{"empty object", `{}`, "application/json", 2, true},
		{"exact byte limit", `{"x":"あ"}`, "application/json; charset=utf-8", 11, true},
		{"byte limit not character count", `{"x":"あ"}`, "application/json", 10, false},
		{"null is not object", `null`, "application/json", 100, false},
		{"array is not object", `[]`, "application/json", 100, false},
		{"malformed", `{`, "application/json", 100, false},
		{"second value", `{} {}`, "application/json", 100, false},
		{"trailing whitespace", "{}\n ", "application/json", 100, true},
		{"wrong media type", `{}`, "text/plain", 100, false},
		{"duplicate keys follow JSON.parse", `{"epoch":1,"epoch":2}`, "application/json", 100, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, problem := DecodeObject(strings.NewReader(test.body), test.contentType, test.limit)
			if (problem == nil) != test.valid {
				t.Fatalf("unexpected decode result: %v", problem)
			}
		})
	}
	object, problem := DecodeObject(strings.NewReader(`{"epoch":9007199254740991}`), "application/json", 100)
	if problem != nil {
		t.Fatal(problem)
	}
	if object["epoch"].(json.Number).String() != "9007199254740991" {
		t.Fatal("epoch spelling lost at the JSON boundary")
	}
	if OnlyKeys(object, "epoch") != nil {
		t.Fatal("valid route key refused")
	}
	if OnlyKeys(object, "host") == nil {
		t.Fatal("unknown route key accepted")
	}
}

func TestUTF16LengthKeepsLegacyTextLimits(t *testing.T) {
	for _, test := range []struct {
		text   string
		length int
	}{
		{"", 0}, {"abc", 3}, {"日本語", 3}, {"😀", 2}, {"あ😀b", 4},
	} {
		if got := UTF16Length(test.text); got != test.length {
			t.Errorf("UTF16Length(%q)=%d, want %d", test.text, got, test.length)
		}
	}
}

func TestFilledTextNeedsVisibleTextWithinLimit(t *testing.T) {
	for _, test := range []struct {
		value any
		max   int
		want  bool
	}{
		{"abc", 3, true}, {"abcd", 3, false}, {"😀", 1, false}, {"😀", 2, true},
		{"", 5, false}, {" \n\t", 5, false}, {3, 5, false}, {nil, 5, false},
	} {
		if got := FilledText(test.value, test.max); got != test.want {
			t.Errorf("FilledText(%#v, %d)=%v, want %v", test.value, test.max, got, test.want)
		}
	}
}

func TestClipUTF16CutsAtJavaScriptLength(t *testing.T) {
	for _, test := range []struct {
		text  string
		limit int
		want  string
	}{
		{"", 5, ""},
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"abcd", 3, "abc"},
		{"日本語です", 3, "日本語"},
		{"a😀b", 1, "a"},
		{"a😀b", 2, "a"},
		{"a😀b", 3, "a😀"},
		{"a😀b", 4, "a😀b"},
		{"😀😀", 3, "😀"},
		{"a\xffb", 2, "a\ufffd"},
	} {
		got := ClipUTF16(test.text, test.limit)
		if got != test.want {
			t.Errorf("ClipUTF16(%q, %d)=%q, want %q", test.text, test.limit, got, test.want)
		}
		if test.limit >= 0 && UTF16Length(got) > test.limit {
			t.Errorf("ClipUTF16(%q, %d) is %d units long", test.text, test.limit, UTF16Length(got))
		}
	}
}
