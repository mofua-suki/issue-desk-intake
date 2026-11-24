package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const JobsBodyLimit int64 = 128 * 1024

// DecodeObject implements the existing jobs API's JSON-object boundary. Size is
// measured in received bytes, numbers retain their spelling, and a second JSON
// value is rejected. Duplicate keys retain JSON.parse's last-value behavior.
func DecodeObject(reader io.Reader, contentType string, maxBytes int64) (map[string]any, *Problem) {
	if !strings.HasPrefix(contentType, "application/json") {
		return nil, NewProblem(400, "JSONで送ってください。", "")
	}
	if maxBytes <= 0 || maxBytes == int64(^uint64(0)>>1) {
		return nil, NewProblem(500, "本文の上限設定が不正です。", "")
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, NewProblem(400, "本文を読み取れません。", "")
	}
	if int64(len(body)) > maxBytes {
		return nil, NewProblem(400, fmt.Sprintf("本文は %d bytes 以内にしてください。", maxBytes), "")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, NewProblem(400, "本文をJSONとして読めません。", "")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, NewProblem(400, "本文をJSONとして読めません。", "")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, NewProblem(400, "本文は JSON のオブジェクトにしてください。", "")
	}
	return object, nil
}

// OnlyKeys prevents route bodies from silently accepting an unknown operation.
// Callers list each endpoint's accepted keys from its legacy implementation.
func OnlyKeys(object map[string]any, allowed ...string) *Problem {
	for key := range object {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return NewProblem(400, "本文に使えない項目があります。", "")
		}
	}
	return nil
}

// FilledText reports whether value is a string that is not blank and is at
// most max UTF-16 code units long.
func FilledText(value any, max int) bool {
	v, ok := value.(string)
	return ok && strings.TrimSpace(v) != "" && UTF16Length(v) <= max
}

// UTF16Length matches JavaScript String.length. In particular, an emoji counts
// as two code units, while a Japanese BMP character counts as one. HTTP byte
// limits must use the original bytes instead of this helper.
func UTF16Length(value string) int {
	length := 0
	for _, r := range value {
		length++
		if r > 0xffff {
			length++
		}
	}
	return length
}

// ClipUTF16 keeps the leading characters of value whose UTF-16 length (the
// same count as UTF16Length) is at most limit. A character outside the BMP
// that would cross the limit is dropped whole, and limit<=0 returns "".
// Like the JavaScript-compatible copies it replaces, the result is rebuilt
// from runes, so invalid UTF-8 bytes become U+FFFD.
func ClipUTF16(value string, limit int) string {
	var out strings.Builder
	used := 0
	for _, r := range value {
		size := 1
		if r > 0xffff {
			size = 2
		}
		if used+size > limit {
			break
		}
		out.WriteRune(r)
		used += size
	}
	return out.String()
}
