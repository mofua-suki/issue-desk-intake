package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// baselineSpec reproduces the fixed revision's request-time read and filtering.
func baselineSpec(read func(string) ([]byte, error), parse func([]byte) (map[string]json.RawMessage, map[string]json.RawMessage, error), filename, prefix string) ([]byte, error) {
	data, err := read(filename)
	if err != nil || prefix == "" {
		return data, err
	}
	spec, paths, err := parse(data)
	if err != nil {
		return nil, err
	}
	for p := range paths {
		if !strings.HasPrefix(p, prefix) {
			delete(paths, p)
		}
	}
	spec["paths"], err = json.Marshal(paths)
	if err != nil {
		return nil, err
	}
	return json.Marshal(spec)
}

func TestSpecCacheMatchesBaseline(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c := newSpecCache(file, raw)
	var reads, parses atomic.Int64
	c.read = func(name string) ([]byte, error) { reads.Add(1); return os.ReadFile(name) }
	c.parse = func(data []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
		parses.Add(1)
		return parseSpec(data)
	}
	compare := func(prefix string) {
		t.Helper()
		got, ge := c.load(prefix)
		want, we := baselineSpec(os.ReadFile, parseSpec, file, prefix)
		if (ge != nil) != (we != nil) || !bytes.Equal(got, want) {
			t.Errorf("prefix %q: got error %v, want %v; output differs: %v", prefix, ge, we, !bytes.Equal(got, want))
		}
	}
	prefixes := []string{"/api/issues", "/api/", "/missing", ""}
	for _, p := range prefixes {
		compare(p)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(p string) { defer wg.Done(); compare(p) }(prefixes[i%len(prefixes)])
	}
	wg.Wait()
	if reads.Load() != 36 || parses.Load() != 1 {
		t.Fatalf("reads=%d parses=%d", reads.Load(), parses.Load())
	}
	for _, content := range []string{"{", `{"paths":null}`, `{"paths":[]}`, `{"paths":{"/new":{}}}`} {
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		compare("")
		compare("/new")
		compare("/other")
	}
	// An I/O failure must be returned even when the previous bytes would match.
	c.read = func(string) ([]byte, error) { return nil, errors.New("injected read failure") }
	if _, err := c.load("/new"); err == nil {
		t.Fatal("read failure hidden by cache")
	}
	if _, err := c.load(""); err == nil {
		t.Fatal("unfiltered read failure hidden")
	}
	c.read = os.ReadFile
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	compare("")
	compare("/new")
}

func BenchmarkSpecRead(b *testing.B) {
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		b.Fatal(err)
	}
	file := filepath.Join(b.TempDir(), "openapi.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"baseline", "cache"} {
		b.Run(mode, func(b *testing.B) {
			var reads, parses atomic.Int64
			read := func(name string) ([]byte, error) { reads.Add(1); return os.ReadFile(name) }
			parse := func(data []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
				parses.Add(1)
				return parseSpec(data)
			}
			c := newSpecCache(file, raw)
			c.read = read
			c.parse = parse
			prefixes := []string{"/api/issues", "/api/", "/missing", ""}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if mode == "baseline" {
					_, err = baselineSpec(read, parse, file, prefixes[i%len(prefixes)])
				} else {
					_, err = c.load(prefixes[i%len(prefixes)])
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(reads.Load())/float64(b.N), "reads/op")
			b.ReportMetric(float64(parses.Load())/float64(b.N), "parses/op")
		})
	}
}

func BenchmarkSpecReadConcurrent(b *testing.B) {
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		b.Fatal(err)
	}
	file := filepath.Join(b.TempDir(), "openapi.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"baseline", "cache"} {
		b.Run(mode, func(b *testing.B) {
			var reads, parses, sequence atomic.Int64
			read := func(name string) ([]byte, error) { reads.Add(1); return os.ReadFile(name) }
			parse := func(data []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
				parses.Add(1)
				return parseSpec(data)
			}
			c := newSpecCache(file, raw)
			c.read = read
			c.parse = parse
			prefixes := []string{"/api/issues", "/api/", "/missing", ""}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					p := prefixes[int(sequence.Add(1)-1)%len(prefixes)]
					var err error
					if mode == "baseline" {
						_, err = baselineSpec(read, parse, file, p)
					} else {
						_, err = c.load(p)
					}
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(reads.Load())/float64(b.N), "reads/op")
			b.ReportMetric(float64(parses.Load())/float64(b.N), "parses/op")
		})
	}
}
