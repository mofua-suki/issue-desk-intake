package ai

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

var fieldNamePattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)
var fieldIndexPattern = regexp.MustCompile(`^[0-9]+$`)

var schemaNamesCache = struct {
	sync.Mutex
	entries map[string]func(string) bool
}{entries: make(map[string]func(string) bool)}

// schemaNames caches a bounded number of trusted schema documents.
// The returned function exposes membership checks without exposing the cached map.
func schemaNames(schemaBytes []byte) func(string) bool {
	key := string(schemaBytes)
	schemaNamesCache.Lock()
	defer schemaNamesCache.Unlock()
	if names, ok := schemaNamesCache.entries[key]; ok {
		return names
	}
	var doc any
	if json.Unmarshal(schemaBytes, &doc) != nil {
		return nil
	}
	names := map[string]bool{}
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if props, ok := x["properties"].(map[string]any); ok {
				for k := range props {
					names[k] = true
				}
			}
			for _, child := range x {
				collect(child)
			}
		case []any:
			for _, child := range x {
				collect(child)
			}
		}
	}
	collect(doc)
	if len(schemaNamesCache.entries) >= 32 {
		clear(schemaNamesCache.entries)
	}
	contains := func(name string) bool { return names[name] }
	schemaNamesCache.entries[key] = contains
	return contains
}

// validationField exposes only property names declared in the trusted schema.
func validationField(schema *jsonschema.Schema, schemaBytes, data []byte) string {
	if schema == nil {
		return ""
	}
	names := schemaNames(schemaBytes)
	if names == nil {
		return ""
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	validation, ok := schema.Validate(value).(*jsonschema.ValidationError)
	if !ok {
		return ""
	}
	fields := map[string]bool{}
	var visit func(*jsonschema.ValidationError)
	visit = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				visit(c)
			}
			return
		}
		path := append([]string{}, e.InstanceLocation...)
		if required, ok := e.ErrorKind.(*kind.Required); ok {
			if len(required.Missing) != 1 {
				return
			}
			path = append(path, required.Missing[0])
		}
		parts := []string{}
		for _, p := range path {
			if fieldIndexPattern.MatchString(p) {
				continue
			}
			if !names(p) || !fieldNamePattern.MatchString(p) {
				return
			}
			parts = append(parts, p)
		}
		field := strings.Join(parts, ".")
		if field != "" && len(field) <= 80 {
			fields[field] = true
		}
	}
	visit(validation)
	if len(fields) == 1 {
		for f := range fields {
			return f
		}
	}
	return ""
}
