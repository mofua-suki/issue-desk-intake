// Reference: delta.go at 32ede672b60cc659ce86af1256542c2fc720ac5d.
// Original SHA-256: 844386e6fc620b3536cd2339699787e77aba0d85ede9c0d73e3025a6dadfe4f2.
// Only top-level names/types are adapted to coexist with production code.
package screenapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type referenceBaseline struct {
	scope string
	at    time.Time
	rows  map[string]string
	aux   map[string]string
	hosts *string
	bytes int
}
type referenceDelta struct {
	o     DeltaOptions
	mu    sync.Mutex
	held  map[string]*referenceBaseline
	order []string
	bytes int
}

var referenceAuxiliary = []string{"reviews", "fullTests", "stalls", "attentionAlerts"}
var referenceCursorFormat = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func referenceNewDelta(o DeltaOptions) *referenceDelta {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TTL <= 0 {
		o.TTL = 5 * time.Minute
	}
	if o.Limit <= 0 {
		o.Limit = 64
	}
	if o.Bytes <= 0 {
		o.Bytes = 16 << 20
	}
	return &referenceDelta{o: o, held: map[string]*referenceBaseline{}}
}
func referenceSignature(v any) string { b, _ := json.Marshal(v); return string(b) }
func referenceHostSignature(v any) string {
	if v == nil {
		return "null"
	}
	m := clone(fleet.ObjectOrEmpty(v))
	delete(m, "checkedAt")
	return referenceSignature(m)
}
func (d *referenceDelta) drop(cursor string) {
	if b := d.held[cursor]; b != nil {
		d.bytes -= b.bytes
		delete(d.held, cursor)
	}
	for i, key := range d.order {
		if key == cursor {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}
func (d *referenceDelta) expire() {
	now := d.o.Now()
	for _, key := range append([]string{}, d.order...) {
		if b := d.held[key]; b != nil && now.Sub(b.at) > d.o.TTL {
			d.drop(key)
		}
	}
}
func (d *referenceDelta) referenceBaseline(scope string, s Snapshot) *referenceBaseline {
	b := &referenceBaseline{scope: scope, at: d.o.Now(), rows: map[string]string{}, aux: map[string]string{}}
	for _, r := range s.Rows {
		sig := referenceSignature(r.Item)
		b.rows[r.Key] = sig
		b.bytes += len(r.Key) + len(sig)
	}
	for _, name := range referenceAuxiliary {
		if value, has := s.Aux[name]; has {
			sig := referenceSignature(value)
			b.aux[name] = sig
			b.bytes += len(sig)
		}
	}
	if s.Hosts != nil {
		sig := referenceHostSignature(s.Hosts)
		b.hosts = &sig
		b.bytes += len(sig)
	}
	return b
}
func (d *referenceDelta) keep(b *referenceBaseline) any {
	d.expire()
	if b.bytes > d.o.Bytes {
		return nil
	}
	for len(d.order) > 0 && (len(d.order) >= d.o.Limit || d.bytes+b.bytes > d.o.Bytes) {
		d.drop(d.order[0])
	}
	var raw [18]byte
	if _, e := rand.Read(raw[:]); e != nil {
		return nil
	}
	cursor := "l1." + base64.RawURLEncoding.EncodeToString(raw[:])
	d.held[cursor] = b
	d.order = append(d.order, cursor)
	d.bytes += b.bytes
	return cursor
}
func referenceUnavailable(s Snapshot) []string {
	out := []string{}
	for _, name := range referenceAuxiliary {
		if _, has := s.Aux[name]; !has {
			out = append(out, name)
		}
	}
	if s.Hosts == nil {
		out = append(out, "hosts")
	}
	return out
}
func (d *referenceDelta) full(scope string, s Snapshot, reset bool) Object {
	aux := Object{}
	for _, name := range referenceAuxiliary {
		if value, has := s.Aux[name]; has {
			aux[name] = value
		}
	}
	out := Object{"cursor": d.keep(d.referenceBaseline(scope, s)), "full": true, "reset": reset, "rows": s.Rows, "aux": aux, "unavailable": referenceUnavailable(s)}
	if s.Hosts != nil {
		out["hosts"] = s.Hosts
	}
	return out
}
func (d *referenceDelta) Respond(ctx context.Context, scope string, cursor *string, load func(context.Context) (Snapshot, error)) (Object, error) {
	if cursor != nil && !referenceCursorFormat.MatchString(*cursor) {
		return nil, fail(400, "sinceの形式が不正です。")
	}
	d.mu.Lock()
	d.expire()
	var old *referenceBaseline
	if cursor != nil && strings.HasPrefix(*cursor, "l1.") {
		old = d.held[*cursor]
	}
	if old != nil && old.scope != scope {
		old = nil
	}
	d.mu.Unlock()
	s, e := load(ctx)
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	for _, r := range s.Rows {
		if r.Key == "" || seen[r.Key] {
			return nil, fail(503, "一覧の行の識別子を確認できません。")
		}
		seen[r.Key] = true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire()
	if old == nil || cursor == nil || d.held[*cursor] != old {
		return d.full(scope, s, cursor != nil), nil
	}
	added, updated := []Row{}, []Row{}
	removed := []string{}
	for _, r := range s.Rows {
		before, exists := old.rows[r.Key]
		if !exists {
			added = append(added, r)
		} else if before != referenceSignature(r.Item) {
			updated = append(updated, r)
		}
	}
	for key := range old.rows {
		if !seen[key] {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	next := d.referenceBaseline(scope, s)
	aux := Object{}
	for _, name := range referenceAuxiliary {
		if value, has := s.Aux[name]; !has {
			if before, exists := old.aux[name]; exists {
				next.aux[name] = before
				next.bytes += len(before)
			}
		} else if old.aux[name] != next.aux[name] {
			aux[name] = value
		}
	}
	hostChanged := false
	if s.Hosts == nil {
		if old.hosts != nil {
			sig := *old.hosts
			next.hosts = &sig
			next.bytes += len(sig)
		}
	} else {
		hostChanged = old.hosts == nil || *old.hosts != *next.hosts
	}
	d.drop(*cursor)
	nextCursor := d.keep(next)
	if nextCursor == nil {
		return d.full(scope, s, true), nil
	}
	out := Object{"cursor": nextCursor, "full": false, "reset": false, "added": added, "updated": updated, "removed": removed, "aux": aux, "unavailable": referenceUnavailable(s)}
	if hostChanged {
		out["hosts"] = s.Hosts
	}
	return out, nil
}
