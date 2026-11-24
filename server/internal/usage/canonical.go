package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

func quote(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 32 {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

func number(value json.Number) (string, error) {
	v, err := value.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", errors.New("invalid JSON number")
	}
	if v == 0 {
		return "0", nil
	}
	abs := math.Abs(v)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}
	parts := strings.Split(strconv.FormatFloat(v, 'e', -1, 64), "e")
	exponent, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", err
	}
	sign := ""
	if exponent >= 0 {
		sign = "+"
	}
	return parts[0] + "e" + sign + strconv.Itoa(exponent), nil
}

// Compact preserves object property order while applying JSON.stringify's
// escaping and number formatting, which are part of persisted row identities.
func Compact(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func() (string, error)
	value = func() (string, error) {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch v := token.(type) {
		case nil:
			return "null", nil
		case bool:
			if v {
				return "true", nil
			}
			return "false", nil
		case string:
			return quote(v), nil
		case json.Number:
			return number(v)
		case json.Delim:
			parts := []string{}
			switch v {
			case '{':
				names := []string{}
				values := map[string]string{}
				for decoder.More() {
					key, err := decoder.Token()
					if err != nil {
						return "", err
					}
					name, ok := key.(string)
					if !ok {
						return "", errors.New("invalid JSON key")
					}
					item, err := value()
					if err != nil {
						return "", err
					}
					if _, exists := values[name]; !exists {
						names = append(names, name)
					}
					values[name] = item
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim('}') {
					return "", errors.New("invalid JSON object")
				}
				indices := []string{}
				others := []string{}
				for _, name := range names {
					if _, ok := arrayIndex(name); ok {
						indices = append(indices, name)
					} else {
						others = append(others, name)
					}
				}
				sort.Slice(indices, func(i, j int) bool { a, _ := arrayIndex(indices[i]); b, _ := arrayIndex(indices[j]); return a < b })
				for _, name := range append(indices, others...) {
					parts = append(parts, quote(name)+":"+values[name])
				}
				return "{" + strings.Join(parts, ",") + "}", nil
			case '[':
				for decoder.More() {
					item, err := value()
					if err != nil {
						return "", err
					}
					parts = append(parts, item)
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim(']') {
					return "", errors.New("invalid JSON array")
				}
				return "[" + strings.Join(parts, ",") + "]", nil
			}
		}
		return "", errors.New("invalid JSON")
	}
	result, err := value()
	if err != nil {
		return "", err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return "", errors.New("trailing JSON value")
	}
	return result, nil
}

func OrderedRow(raw map[string]json.RawMessage, fields []string) (string, error) {
	parts := []string{}
	for _, field := range fields {
		value, exists := raw[field]
		if !exists {
			continue
		}
		text, err := Compact(value)
		if err != nil {
			return "", err
		}
		parts = append(parts, quote(field)+":"+text)
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

func arrayIndex(name string) (uint64, bool) {
	v, e := strconv.ParseUint(name, 10, 32)
	return v, e == nil && v < 4294967295 && strconv.FormatUint(v, 10) == name
}
