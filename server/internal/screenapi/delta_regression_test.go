package screenapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countedJSON struct {
	calls   *atomic.Int64
	value   any
	entered chan<- struct{}
	release <-chan struct{}
}

func (v countedJSON) MarshalJSON() ([]byte, error) {
	v.calls.Add(1)
	if v.entered != nil {
		v.entered <- struct{}{}
		<-v.release
	}
	return json.Marshal(v.value)
}
func countedSnapshot(calls *atomic.Int64) Snapshot {
	s := deltaSnapshot()
	s.Rows[0].Item["counted"] = countedJSON{calls: calls, value: "行"}
	for _, name := range auxiliary {
		s.Aux[name] = countedJSON{calls: calls, value: []any{}}
	}
	s.Hosts = Object{"checkedAt": "1", "hosts": countedJSON{calls: calls, value: []any{}}}
	return s
}
func TestDeltaMaterialGeneratedOnce(t *testing.T) {
	for _, mode := range []string{"initial", "delta", "expired", "unknown", "oversize", "carried-overflow"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			now := time.Unix(0, 0)
			d := NewDelta(DeltaOptions{Now: func() time.Time { return now }})
			s := countedSnapshot(&calls)
			load := func(context.Context) (Snapshot, error) { return s, nil }
			var cursor *string
			if mode != "initial" {
				v, err := d.Respond(context.Background(), "a", nil, load)
				if err != nil {
					t.Fatal(err)
				}
				c := fleet.Text(v["cursor"])
				cursor = &c
			}
			switch mode {
			case "expired":
				now = now.Add(6 * time.Minute)
			case "unknown":
				*cursor = "l1.unknown"
			case "oversize":
				d.o.Bytes = 1
			case "carried-overflow":
				d.o.Bytes = d.held[*cursor].bytes - 1
				delete(s.Aux, "reviews")
			}
			calls.Store(0)
			v, err := d.Respond(context.Background(), "a", cursor, load)
			want := int64(6)
			if mode == "carried-overflow" {
				want--
			}
			if err != nil || calls.Load() != want {
				t.Fatalf("%v calls=%d want=%d: %v", v, calls.Load(), want, err)
			}
			if v["full"] != (mode != "delta") {
				t.Fatal(v)
			}
			if mode == "carried-overflow" && v["cursor"] == nil {
				t.Fatal("smaller full baseline should fit", v)
			}
		})
	}
}
func normalizedDelta(v Object) string {
	out := clone(v)
	out["cursor"] = v["cursor"] != nil
	return signature(out)
}
func TestDeltaMatchesFixedReference(t *testing.T) {
	for _, options := range []DeltaOptions{{}, {Limit: 2}, {Bytes: 600}, {Bytes: 1}} {
		t.Run(fmt.Sprintf("limit=%d,bytes=%d", options.Limit, options.Bytes), func(t *testing.T) {
			now := time.Unix(0, 0)
			options.Now = func() time.Time { return now }
			d, ref := NewDelta(options), referenceNewDelta(options)
			var cursor, refCursor *string
			step := func(s Snapshot, scope string) {
				t.Helper()
				load := func(context.Context) (Snapshot, error) { return s, nil }
				v, e := d.Respond(context.Background(), scope, cursor, load)
				w, f := ref.Respond(context.Background(), scope, refCursor, load)
				if (e == nil) != (f == nil) || normalizedDelta(v) != normalizedDelta(w) {
					t.Fatalf("current=%v,%v reference=%v,%v", v, e, w, f)
				}
				c, r := fleet.Text(v["cursor"]), fleet.Text(w["cursor"])
				cursor, refCursor = &c, &r
				if c == "" {
					cursor, refCursor = nil, nil
				}
			}
			s := deltaSnapshot()
			step(s, "a")
			step(s, "a")
			s.Rows = append(s.Rows, Row{"todo:b", "todo", Object{"title": "追加"}}, Row{"todo:c", "todo", Object{"title": "削除"}})
			step(s, "a")
			s.Rows[0].Item["title"] = "更新"
			s.Rows = s.Rows[:2]
			step(s, "a")
			delete(s.Aux, "reviews")
			s.Hosts = nil
			step(s, "a")
			s.Aux["reviews"] = []any{}
			s.Hosts = Object{"checkedAt": "9", "hosts": []any{Object{"host": "nas"}}}
			step(s, "a")
			s.Aux["reviews"] = []any{Object{"id": "changed"}}
			s.Hosts = Object{"checkedAt": "10", "hosts": []any{Object{"host": "new"}}}
			step(s, "a")
			step(s, "b")
			now = now.Add(6 * time.Minute)
			step(s, "b")
			s.Rows = []Row{}
			step(s, "b")
		})
	}
}
func TestDeltaCursorLimitsAndInvalidKeys(t *testing.T) {
	ctx := context.Background()
	s := deltaSnapshot()
	load := func(context.Context) (Snapshot, error) { return s, nil }
	for _, mode := range []string{"count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			base, _ := prepareBaseline("a", s)
			o := DeltaOptions{Limit: 2}
			if mode == "bytes" {
				o = DeltaOptions{Bytes: base.bytes * 2}
			}
			d := NewDelta(o)
			first, _ := d.Respond(ctx, "a", nil, load)
			second, _ := d.Respond(ctx, "b", nil, load)
			d.Respond(ctx, "c", nil, load)
			if len(d.held) != 2 || d.held[fleet.Text(first["cursor"])] != nil || d.held[fleet.Text(second["cursor"])] == nil {
				t.Fatal("FIFO bound", d.order)
			}
			c := fleet.Text(first["cursor"])
			v, err := d.Respond(ctx, "a", &c, load)
			if err != nil || v["reset"] != true || v["full"] != true {
				t.Fatal(v, err)
			}
		})
	}
	for _, rows := range [][]Row{{{"", "draft", Object{}}}, {{"x", "draft", Object{}}, {"x", "draft", Object{}}}} {
		d := NewDelta(DeltaOptions{})
		first, _ := d.Respond(ctx, "a", nil, load)
		c := fleet.Text(first["cursor"])
		_, err := d.Respond(ctx, "a", &c, func(context.Context) (Snapshot, error) { return Snapshot{Rows: rows}, nil })
		if err == nil || d.held[c] == nil {
			t.Fatal("invalid keys consumed cursor", err)
		}
	}
	config := Object{"repositories": []any{Object{"name": "a"}}}
	original := sessionScope("cookie-a", config, "origin-a")
	for _, other := range []string{sessionScope("cookie-b", config, "origin-a"), sessionScope("cookie-a", config, "origin-b"), sessionScope("cookie-a", Object{}, "origin-a")} {
		d := NewDelta(DeltaOptions{})
		v, _ := d.Respond(ctx, original, nil, load)
		c := fleet.Text(v["cursor"])
		v, err := d.Respond(ctx, other, &c, load)
		if err != nil || v["reset"] != true {
			t.Fatal(v, err)
		}
	}
}
func TestDeltaPreparationDoesNotHoldSharedLockAndRevalidates(t *testing.T) {
	for _, mode := range []string{"consume", "expire", "evict"} {
		t.Run(mode, func(t *testing.T) {
			var clock atomic.Int64
			d := NewDelta(DeltaOptions{Now: func() time.Time { return time.Unix(clock.Load(), 0) }, Limit: 2})
			ctx := context.Background()
			plain := deltaSnapshot()
			load := func(context.Context) (Snapshot, error) { return plain, nil }
			v, _ := d.Respond(ctx, "a", nil, load)
			cursor := fleet.Text(v["cursor"])
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int64
			s := countedSnapshot(&calls)
			delete(s.Aux, "reviews")
			s.Hosts = nil
			s.Rows[0].Item["gate"] = countedJSON{calls: &calls, value: "blocked", entered: entered, release: release}
			type result struct {
				v   Object
				err error
			}
			done := make(chan result, 1)
			go func() {
				v, err := d.Respond(ctx, "a", &cursor, func(context.Context) (Snapshot, error) { return s, nil })
				done <- result{v, err}
			}()
			<-entered // old acquired, JSON preparation suspended.
			switch mode {
			case "consume":
				v, err := d.Respond(ctx, "a", &cursor, load)
				if err != nil || v["full"] != false {
					t.Error(v, err)
				}
			case "expire":
				clock.Store(301)
			case "evict":
				d.Respond(ctx, "b", nil, load)
				d.Respond(ctx, "c", nil, load)
			}
			// Taking the mutex while MarshalJSON is suspended proves it is outside it.
			if !d.mu.TryLock() {
				close(release)
				t.Fatal("JSON preparation holds the mutex")
			}
			d.mu.Unlock()
			close(release)
			r := <-done
			if r.err != nil || r.v["full"] != true || r.v["reset"] != true || calls.Load() != 5 {
				t.Fatal(r, calls.Load())
			}
			b := d.held[fleet.Text(r.v["cursor"])]
			if b == nil || b.hosts != nil {
				t.Fatal("fallback baseline missing or carried stale hosts")
			}
			if _, carried := b.aux["reviews"]; carried {
				t.Fatal("full fallback retained unavailable aux")
			}
		})
	}
}
func TestDeltaLosslessCompactionAndLogicalCapacity(t *testing.T) {
	for _, values := range [][]string{{"", ""}, {"same", "same"}, {"異なる", "別"}, {strings.Repeat("共通", 60) + "a" + strings.Repeat("後", 60), strings.Repeat("共通", 60) + "b" + strings.Repeat("後", 60)}} {
		s := Snapshot{Rows: []Row{}, Aux: map[string]any{}}
		for i, value := range values {
			s.Rows = append(s.Rows, Row{"draft:" + fmt.Sprint(i), "draft", Object{"title": value}})
		}
		b, _ := prepareBaseline("a", s)
		original := make(map[string]string)
		for key, sig := range b.rows {
			original[key] = sig
		}
		logical := b.bytes
		b.compactRows()
		for key, sig := range original {
			if !b.rowEqual(key, b.rows[key], sig) || b.rowEqual(key, b.rows[key], sig+" ") {
				t.Fatal("lossy comparison", key)
			}
		}
		if b.bytes != logical {
			t.Fatal("logical capacity changed")
		}
		if len(values[0]) > 100 && len(b.affixes) == 0 {
			t.Fatal("no compaction")
		}
		load := func(context.Context) (Snapshot, error) { return s, nil }
		d := NewDelta(DeltaOptions{Bytes: logical - 1})
		v, err := d.Respond(context.Background(), "a", nil, load)
		if err != nil || v["cursor"] != nil {
			t.Fatal("compressed bytes relaxed bound", v, err)
		}
		d = NewDelta(DeltaOptions{Bytes: logical})
		v, err = d.Respond(context.Background(), "a", nil, load)
		if err != nil || v["cursor"] == nil {
			t.Fatal("exact boundary", v, err)
		}
		c := fleet.Text(v["cursor"])
		s.Rows[0].Item["title"] = values[0] + "変更"
		v, err = d.Respond(context.Background(), "a", &c, load)
		if err != nil || v["full"] != true || signature(v["rows"]) != signature(s.Rows) {
			t.Fatal("overflow full differs", v, err)
		}
	}
}

// Exercise the same snapshot producer used by service.go, including all shipped
// projections, permission filtering, read failures, and unavailable aux/hosts.
func TestDeltaProductionSnapshotFlow(t *testing.T) {
	source := Object{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "title": "元", "status": "draft", "body": "本文は一覧に含めない"}
	missing := false
	service := &Service{o: Options{
		Config: func() (Object, error) { return Object{"repositories": []any{Object{"name": "owner/repo"}}}, nil },
		Lists:  map[string]ListReader{}, Aux: map[string]func(context.Context) (any, error){},
	}}
	for _, kind := range rowTypes {
		service.o.Lists[kind] = func(context.Context, string) ([]Object, error) { return []Object{}, nil }
	}
	service.o.Lists["draft"] = func(context.Context, string) ([]Object, error) {
		return []Object{source, {"id": "22222222-2222-4222-8222-222222222222", "project": "other/repo", "title": "許可外"}}, nil
	}
	for _, name := range auxiliary {
		service.o.Aux[name] = func(context.Context) (any, error) {
			if missing {
				return nil, fmt.Errorf("unavailable")
			}
			return []any{}, nil
		}
	}
	service.o.Aux["hosts"] = func(context.Context) (any, error) {
		if missing {
			return nil, fmt.Errorf("unavailable")
		}
		return Object{"checkedAt": "now", "hosts": []any{}}, nil
	}
	d := NewDelta(DeltaOptions{})
	load := func(ctx context.Context) (Snapshot, error) { return service.snapshot(ctx, "origin", newListTiming()) }
	v, err := d.Respond(context.Background(), "scope", nil, load)
	if err != nil || len(fleet.Items(v["rows"])) != 1 || strings.Contains(signature(v), "本文") || strings.Contains(signature(v), "許可外") {
		t.Fatal(v, err)
	}
	c := fleet.Text(v["cursor"])
	source["title"] = "更新"
	missing = true
	v, err = d.Respond(context.Background(), "scope", &c, load)
	if err != nil || v["full"] != false || len(fleet.Items(v["updated"])) != 1 || len(fleet.Items(v["unavailable"])) != 5 {
		t.Fatal(v, err)
	}
	c = fleet.Text(v["cursor"])
	missing = false
	v, err = d.Respond(context.Background(), "scope", &c, load)
	if err != nil || len(fleet.ObjectOrEmpty(v["aux"])) != 0 || v["hosts"] != nil || len(fleet.Items(v["updated"])) != 0 {
		t.Fatal(v, err)
	}
}

func TestDeltaExpiryBoundaryAndImmutableCarry(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDelta(DeltaOptions{Now: func() time.Time { return now }})
	s := deltaSnapshot()
	load := func(context.Context) (Snapshot, error) { return s, nil }
	v, _ := d.Respond(context.Background(), "a", nil, load)
	c := fleet.Text(v["cursor"])
	old := d.held[c]
	before := signature(old.aux)
	hosts := *old.hosts
	now = now.Add(5 * time.Minute)
	delete(s.Aux, "reviews")
	s.Hosts = nil
	v, err := d.Respond(context.Background(), "a", &c, load)
	if err != nil || v["full"] != false {
		t.Fatal("TTL equality must stay valid", v, err)
	}
	if signature(old.aux) != before || *old.hosts != hosts {
		t.Fatal("published baseline mutated")
	}
	c = fleet.Text(v["cursor"])
	now = now.Add(5*time.Minute + time.Nanosecond)
	v, err = d.Respond(context.Background(), "a", &c, load)
	if err != nil || v["reset"] != true {
		t.Fatal("TTL strictly exceeded", v, err)
	}
}

func TestDeltaUnavailableRecoveryChangedValues(t *testing.T) {
	for _, initialMissing := range []bool{false, true} {
		t.Run(fmt.Sprint(initialMissing), func(t *testing.T) {
			d := NewDelta(DeltaOptions{})
			s := deltaSnapshot()
			if initialMissing {
				delete(s.Aux, "reviews")
				s.Hosts = nil
			}
			load := func(context.Context) (Snapshot, error) { return s, nil }
			v, err := d.Respond(context.Background(), "a", nil, load)
			if err != nil {
				t.Fatal(err)
			}
			c := fleet.Text(v["cursor"])
			if !initialMissing {
				delete(s.Aux, "reviews")
				s.Hosts = nil
				v, err = d.Respond(context.Background(), "a", &c, load)
				if err != nil || v["full"] != false || v["hosts"] != nil || len(fleet.ObjectOrEmpty(v["aux"])) != 0 {
					t.Fatal(v, err)
				}
				c = fleet.Text(v["cursor"])
			}
			s.Aux["reviews"] = []any{Object{"id": "new"}}
			s.Hosts = Object{"checkedAt": "3", "hosts": []any{Object{"host": "new"}}}
			v, err = d.Respond(context.Background(), "a", &c, load)
			if err != nil || v["full"] != false || signature(fleet.ObjectOrEmpty(v["aux"])["reviews"]) != signature(s.Aux["reviews"]) || signature(v["hosts"]) != signature(s.Hosts) || len(fleet.Items(v["unavailable"])) != 0 {
				t.Fatal(v, err)
			}
		})
	}
}
