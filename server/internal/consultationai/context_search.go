package consultationai

import (
	"context"
	"fmt"
	"sync"
)

const searchReadConcurrency = 4

// Search fetches a small window concurrently but consumes it in path order.
// The window bounds retained sources as well as requests. Cancellation and the
// first error in path order stop the search and join every outstanding fetch.
func (m *materials) searchSources(ctx context.Context, p *projectState, files []string, consume func(string, string) error) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	type result struct {
		source string
		err    error
	}
	results := make([]chan result, min(searchReadConcurrency, len(files)))
	start := func(i int) {
		entry := p.entries[files[i]]
		output := make(chan result, 1)
		results[i%len(results)] = output
		if entry.Size > maxSource {
			output <- result{}
			return
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := ctx.Err(); err != nil {
				output <- result{err: err}
				return
			}
			source, err := m.blob(ctx, p, entry)
			output <- result{source, err}
		}()
	}
	for i := range results {
		start(i)
	}
	for i, file := range files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case got := <-results[i%len(results)]:
			if err := ctx.Err(); err != nil {
				return err
			}
			if got.err != nil {
				return fmt.Errorf("資料検索 %s: %w", file, got.err)
			}
			if p.entries[file].Size <= maxSource {
				if err := consume(file, got.source); err != nil {
					return err
				}
			}
		}
		if next := i + len(results); next < len(files) {
			start(next)
		}
	}
	return ctx.Err()
}
