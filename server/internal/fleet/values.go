// 設定・割り当ての汎用の値（map・文字列・数・一覧）の読み替え、複製、JSONの読み込みと要約値。

package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
)

func Object(value any) map[string]any {
	if v, ok := value.(map[string]any); ok {
		return v
	}
	// Named maps such as consultations.Record have the same representation but
	// do not satisfy a map[string]any type assertion until converted.
	v := reflect.ValueOf(value)
	target := reflect.TypeOf(map[string]any(nil))
	if v.IsValid() && v.Kind() == reflect.Map && v.Type().ConvertibleTo(target) {
		return v.Convert(target).Interface().(map[string]any)
	}
	return nil
}

// ObjectOrEmpty is Object, but never nil, so callers may write into the result.
func ObjectOrEmpty(value any) map[string]any {
	if v := Object(value); v != nil {
		return v
	}
	return map[string]any{}
}

func Text(value any) string { v, _ := value.(string); return v }

func Number(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		n, _ := v.Float64()
		return n
	}
	return 0
}

func List(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = x
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = x
		}
		return out
	default:
		return nil
	}
}

// Elements reads any slice or array, such as []Record, element by element
// without converting the elements.
func Elements(value any) []any {
	if v, ok := value.([]any); ok {
		return v
	}
	r := reflect.ValueOf(value)
	if !r.IsValid() || (r.Kind() != reflect.Slice && r.Kind() != reflect.Array) {
		return nil
	}
	out := make([]any, r.Len())
	for i := range out {
		out[i] = r.Index(i).Interface()
	}
	return out
}

// Items reads a value as a JSON array: values other than []any are encoded and
// decoded, so typed elements become their JSON form. Non-arrays give nil.
func Items(value any) []any {
	if v, ok := value.([]any); ok {
		return v
	}
	raw, _ := json.Marshal(value)
	var out []any
	_ = json.Unmarshal(raw, &out)
	return out
}

// Int64 truncates a number to int64 (an int64 keeps all its digits).
func Int64(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// WholeNumber reads an integer; a fractional float64 or a json.Number that is
// not an int64 gives 0.
func WholeNumber(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	}
	return 0
}

func Strings(value any) []string {
	result := []string{}
	for _, item := range List(value) {
		if v, ok := item.(string); ok {
			result = append(result, v)
		}
	}
	return result
}

func Keys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func Clone(value any) any {
	raw, _ := json.Marshal(value)
	var copy any
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func ReadJSON(file string, target any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), target)
}

func Digest(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return ""
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}))
	return hex.EncodeToString(sum[:])[:16]
}

func KeysBool(value map[string]bool) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
