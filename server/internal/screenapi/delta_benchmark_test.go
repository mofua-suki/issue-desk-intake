package screenapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// The fixture passes through the shipped projection for all four row kinds.
// Neither timed fixture contains MarshalJSON instrumentation. Counts are probed
// separately with one counted value per row, aux entry, and hosts entry.
func deltaFixture(n int) Snapshot {
	s := deltaSnapshot()
	s.Rows = make([]Row, 0, n)
	for i := 0; i < n; i++ {
		kind := rowTypes[i%len(rowTypes)]
		item := Object{"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "number": i + 1, "project": "example/intake", "title": fmt.Sprintf("受付の性能調査 %06d", i), "status": "reviewing", "createdAt": "2026-11-01T00:00:00Z", "updatedAt": "2026-11-06T12:00:00Z", "actions": Object{"open": true, "retry": false}, "body": "一覧用の短い説明。本文全体は画面へ送らない。", "messages": []any{Object{"role": "user", "text": "一覧の表示と差分の動作を確認する代表的な相談です。"}}}
		projected := Summarize(kind, item)
		key, err := rowKey(kind, projected)
		if err != nil {
			panic(err)
		}
		s.Rows = append(s.Rows, Row{key, kind, projected})
	}
	return s
}
func changedFixture(s Snapshot, percent int) Snapshot {
	out := s
	out.Rows = append([]Row{}, s.Rows...)
	for i := 0; i < len(s.Rows)*percent/100; i++ {
		out.Rows[i].Item = clone(s.Rows[i].Item)
		out.Rows[i].Item["title"] = "変更された受付の性能調査"
	}
	return out
}

type deltaBenchEngine struct {
	respond     func(context.Context, string, *string, func(context.Context) (Snapshot, error)) (Object, error)
	stringsHeld func() int
	logicalHeld func() int
	anchor      any
}

func benchEngine(reference bool, sessions, capacity int) deltaBenchEngine {
	if reference {
		d := referenceNewDelta(DeltaOptions{Limit: 64, Bytes: capacity})
		return deltaBenchEngine{d.Respond, func() int {
			n := 0
			for _, b := range d.held {
				for k, v := range b.rows {
					n += len(k) + len(v)
				}
				for _, v := range b.aux {
					n += len(v)
				}
				if b.hosts != nil {
					n += len(*b.hosts)
				}
			}
			return n
		}, func() int { return d.bytes }, d}
	}
	d := NewDelta(DeltaOptions{Limit: 64, Bytes: capacity})
	return deltaBenchEngine{d.Respond, func() int {
		n := 0
		for _, b := range d.held {
			for k, v := range b.rows {
				n += len(k) + len(v)
			}
			for _, a := range b.affixes {
				n += len(a.prefix) + len(a.suffix)
			}
			for _, v := range b.aux {
				n += len(v)
			}
			if b.hosts != nil {
				n += len(*b.hosts)
			}
		}
		return n
	}, func() int { return d.bytes }, d}
}
func instrumentSnapshot(s Snapshot, calls *atomic.Int64) Snapshot {
	out := s
	out.Rows = append([]Row{}, s.Rows...)
	for i, r := range s.Rows {
		out.Rows[i].Item = clone(r.Item)
		out.Rows[i].Item["title"] = countedJSON{calls: calls, value: r.Item["title"]}
	}
	out.Aux = map[string]any{}
	for name, v := range s.Aux {
		out.Aux[name] = countedJSON{calls: calls, value: v}
	}
	if s.Hosts != nil {
		hosts := clone(fleet.ObjectOrEmpty(s.Hosts))
		hosts["hosts"] = countedJSON{calls: calls, value: hosts["hosts"]}
		out.Hosts = hosts
	}
	return out
}
func fixtureConversions(reference bool, capacity int, base, next Snapshot) int64 {
	var calls atomic.Int64
	first, current := instrumentSnapshot(base, &calls), instrumentSnapshot(next, &calls)
	e := benchEngine(reference, 1, capacity)
	v, err := e.respond(context.Background(), "probe", nil, func(context.Context) (Snapshot, error) { return first, nil })
	if err != nil {
		panic(err)
	}
	var c *string
	if cursor, ok := v["cursor"].(string); ok {
		c = &cursor
	}
	calls.Store(0)
	_, err = e.respond(context.Background(), "probe", c, func(context.Context) (Snapshot, error) { return current, nil })
	if err != nil {
		panic(err)
	}
	return calls.Load()
}

func BenchmarkDeltaComparison(b *testing.B) {
	const rows = 2000
	base := deltaFixture(rows)
	logical, _ := prepareBaseline("fixture", base)
	for _, changed := range []int{0, 1} {
		next := changedFixture(base, changed)
		for _, sessions := range []int{1, 8} {
			for _, overflow := range []bool{false, true} {
				capacity := 16 << 20
				if overflow {
					capacity = logical.bytes / 2
				}
				for _, reference := range []bool{true, false} {
					name := fmt.Sprintf("reference=%t/rows=%d/change=%dpct/sessions=%d/overflow=%t", reference, rows, changed, sessions, overflow)
					b.Run(name, func(b *testing.B) {
						engine := benchEngine(reference, sessions, capacity)
						scopes := make([]string, sessions)
						cursors := make([]*string, sessions)
						for i := range scopes {
							scopes[i] = fmt.Sprintf("session-%d", i)
							v, err := engine.respond(context.Background(), scopes[i], nil, func(context.Context) (Snapshot, error) { return base, nil })
							if err != nil {
								b.Fatal(err)
							}
							if c, ok := v["cursor"].(string); ok {
								cursors[i] = &c
							}
						}
						var full, reset atomic.Int64
						b.ReportAllocs()
						b.ResetTimer()
						var wg sync.WaitGroup
						for worker := 0; worker < sessions; worker++ {
							wg.Add(1)
							go func(worker int) {
								defer wg.Done()
								for j := worker; j < b.N; j += sessions {
									s := base
									if (j/sessions)%2 == 0 {
										s = next
									}
									v, err := engine.respond(context.Background(), scopes[worker], cursors[worker], func(context.Context) (Snapshot, error) { return s, nil })
									if err != nil {
										panic(err)
									}
									if v["full"] == true {
										full.Add(1)
									}
									if v["reset"] == true {
										reset.Add(1)
									}
									cursors[worker] = nil
									if c, ok := v["cursor"].(string); ok {
										cursors[worker] = &c
									}
								}
							}(worker)
						}
						wg.Wait()
						b.StopTimer()
						b.ReportMetric(float64(full.Load())/float64(b.N), "full/op")
						b.ReportMetric(float64(reset.Load())/float64(b.N), "reset/op")
						b.ReportMetric(float64(engine.stringsHeld()), "held-string-B")
						b.ReportMetric(float64(engine.logicalHeld()), "logical-B")
						b.ReportMetric(float64(logical.bytes), "fixture-B")
						b.ReportMetric(float64(capacity), "capacity-B")
						b.ReportMetric(float64(fixtureConversions(reference, capacity, base, next)), "JSON/op")
					})
				}
			}
		}
	}
}

// Measures actual live heap separately from per-operation allocations and exact
// retained string bytes. Fixture storage is allocated before the first GC and
// excluded. The engine is kept alive until after the second heap reading.
func TestDeltaRetainedHeapMeasurement(t *testing.T) {
	base := deltaFixture(2000)
	for _, reference := range []bool{true, false} {
		for _, sessions := range []int{1, 8} {
			// Warm JSON reflection/pool caches before measuring.
			warm := benchEngine(reference, sessions, 16<<20)
			warm.respond(context.Background(), "warm", nil, func(context.Context) (Snapshot, error) { return base, nil })
			warm = deltaBenchEngine{}
			runtime.GC()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			engine := benchEngine(reference, sessions, 16<<20)
			for i := 0; i < sessions; i++ {
				_, err := engine.respond(context.Background(), fmt.Sprint(i), nil, func(context.Context) (Snapshot, error) { return base, nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&after)
			t.Logf("reference=%t rows=2000 sessions=%d capacity=%d logical=%d retained-strings=%d live-heap-difference=%d (GC measurement includes maps/allocator/runtime noise)", reference, sessions, 16<<20, engine.logicalHeld(), engine.stringsHeld(), int64(after.HeapAlloc)-int64(before.HeapAlloc))
			runtime.KeepAlive(engine.anchor)
		}
	}
	runtime.KeepAlive(base)
}

func TestDeltaShippedProjectionMatchesReference(t *testing.T) {
	base := deltaFixture(2000)
	d, ref := NewDelta(DeltaOptions{}), referenceNewDelta(DeltaOptions{})
	var c, r *string
	for _, s := range []Snapshot{base, base, changedFixture(base, 1), base} {
		load := func(context.Context) (Snapshot, error) { return s, nil }
		v, e := d.Respond(context.Background(), "a", c, load)
		w, f := ref.Respond(context.Background(), "a", r, load)
		if e != nil || f != nil || normalizedDelta(v) != normalizedDelta(w) {
			t.Fatal("shipped projection differs", e, f)
		}
		cc, rr := fleet.Text(v["cursor"]), fleet.Text(w["cursor"])
		c, r = &cc, &rr
	}
}

func TestDeltaFixtureInstrumentationPreservesJSON(t *testing.T) {
	base := deltaFixture(2000)
	for _, s := range []Snapshot{base, changedFixture(base, 1)} {
		var calls atomic.Int64
		if signature(s) != signature(instrumentSnapshot(s, &calls)) || calls.Load() != 2005 {
			t.Fatal("instrumentation changed fixture JSON or count", calls.Load())
		}
	}
	for _, reference := range []bool{true, false} {
		want := int64(2005)
		if reference {
			want = 4005
		}
		if got := fixtureConversions(reference, 16<<20, base, changedFixture(base, 1)); got != want {
			t.Fatal(reference, got, want)
		}
		if got := fixtureConversions(reference, 1, base, base); got != 2005 {
			t.Fatal("overflow", reference, got)
		}
	}
}
