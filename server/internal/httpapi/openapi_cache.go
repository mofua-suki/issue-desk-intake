package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
)

// specCache holds only one version. Every request reads the file, including on a
// cache hit, so read errors and changes invisible to file metadata are observed.
type specCache struct {
	mu       sync.Mutex
	filename string
	read     func(string) ([]byte, error)
	parse    func([]byte) (map[string]json.RawMessage, map[string]json.RawMessage, error)
	raw      []byte
	spec     map[string]json.RawMessage
	paths    map[string]json.RawMessage
	parsed   bool
	parseErr error
}

func newSpecCache(filename string, raw []byte) *specCache {
	return &specCache{filename: filename, raw: raw, read: os.ReadFile, parse: parseSpec}
}

func parseSpec(data []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var spec, paths map[string]json.RawMessage
	if json.Unmarshal(data, &spec) != nil || json.Unmarshal(spec["paths"], &paths) != nil || paths == nil {
		return nil, nil, errors.New("invalid specification paths")
	}
	return spec, paths, nil
}

func (c *specCache) load(prefix string) ([]byte, error) {
	data, err := c.read(c.filename)
	if err != nil {
		return nil, err
	}
	if prefix == "" {
		return data, nil
	}
	c.mu.Lock()
	if !bytes.Equal(data, c.raw) {
		c.raw, c.spec, c.paths, c.parsed, c.parseErr = data, nil, nil, false, nil
	}
	if !c.parsed {
		c.spec, c.paths, c.parseErr = c.parse(data)
		c.parsed = true
	}
	savedSpec, savedPaths, parseErr := c.spec, c.paths, c.parseErr
	c.mu.Unlock()
	if parseErr != nil {
		return nil, parseErr
	}
	paths := make(map[string]json.RawMessage)
	for p, value := range savedPaths {
		if strings.HasPrefix(p, prefix) {
			paths[p] = value
		}
	}
	encoded, err := json.Marshal(paths)
	if err != nil {
		return nil, err
	}
	spec := make(map[string]json.RawMessage, len(savedSpec))
	for k, value := range savedSpec {
		spec[k] = value
	}
	spec["paths"] = encoded
	return json.Marshal(spec)
}
