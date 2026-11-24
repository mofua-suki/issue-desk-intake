package screenapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Row struct {
	Key  string `json:"key"`
	Type string `json:"type"`
	Item Object `json:"item"`
}
type Snapshot struct {
	Rows  []Row
	Aux   map[string]any
	Hosts any
	// UnreadProjects lists candidates whose Issue rows are absent because they
	// could not be read. Their previous rows are kept, not removed.
	UnreadProjects []UnreadProject `json:",omitempty"`
}
type DeltaOptions struct {
	Now          func() time.Time
	TTL          time.Duration
	Limit, Bytes int
}
type baseline struct {
	scope   string
	at      time.Time
	rows    map[string]string
	affixes map[string]rowAffix
	aux     map[string]string
	hosts   *string
	bytes   int
	// ownRows is set when rows is not shared with the snapshot's own baseline
	// (rows of unread projects were carried over) and must be compacted alone.
	ownRows bool
}
type Delta struct {
	o     DeltaOptions
	mu    sync.Mutex
	held  map[string]*baseline
	order []string
	bytes int
}

var auxiliary = []string{"reviews", "fullTests", "stalls", "attentionAlerts"}
var cursorFormat = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func NewDelta(o DeltaOptions) *Delta {
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
	return &Delta{o: o, held: map[string]*baseline{}}
}
func signature(v any) string { b, _ := json.Marshal(v); return string(b) }
func hostSignature(v any) string {
	if v == nil {
		return "null"
	}
	m := clone(fleet.ObjectOrEmpty(v))
	delete(m, "checkedAt")
	return signature(m)
}
func sessionScope(cookie string, c Object, origin string) string {
	repos := []string{}
	for _, v := range fleet.Items(c["repositories"]) {
		repos = append(repos, fleet.Text(fleet.ObjectOrEmpty(v)["name"]))
	}
	sort.Strings(repos)
	sum := sha256.Sum256([]byte(cookie + "|" + origin + "|" + strings.Join(repos, ",")))
	return hex.EncodeToString(sum[:])
}
func (d *Delta) drop(cursor string) {
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
func (d *Delta) expire() {
	now := d.o.Now()
	for _, key := range append([]string{}, d.order...) {
		if b := d.held[key]; b != nil && now.Sub(b.at) > d.o.TTL {
			d.drop(key)
		}
	}
}
func (d *Delta) keep(b *baseline) any {
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
func unavailable(s Snapshot) []string {
	out := []string{}
	for _, name := range auxiliary {
		if _, has := s.Aux[name]; !has {
			out = append(out, name)
		}
	}
	if s.Hosts == nil {
		out = append(out, "hosts")
	}
	return out
}
func fullResponse(s Snapshot, cursor any, reset bool) Object {
	aux := Object{}
	for _, name := range auxiliary {
		if value, has := s.Aux[name]; has {
			aux[name] = value
		}
	}
	out := Object{"cursor": cursor, "full": true, "reset": reset, "rows": s.Rows, "aux": aux, "unavailable": unavailable(s)}
	if s.Hosts != nil {
		out["hosts"] = s.Hosts
	}
	if len(s.UnreadProjects) > 0 {
		out["unreadProjects"] = s.UnreadProjects
	}
	return out
}
func (d *Delta) Respond(ctx context.Context, scope string, cursor *string, load func(context.Context) (Snapshot, error)) (Object, error) {
	if cursor != nil && !cursorFormat.MatchString(*cursor) {
		return nil, fail(400, "sinceの形式が不正です。")
	}
	d.mu.Lock()
	d.expire()
	var old *baseline
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
	// Generate and compare immutable material without the shared session lock.
	base, e := prepareBaseline(scope, s)
	if e != nil {
		return nil, e
	}
	var out Object
	next := base
	if old != nil {
		out, next = compareBaseline(old, base, s)
	}
	if base.bytes <= d.o.Bytes {
		base.compactRows()
	}
	if next.ownRows {
		// Carried rows of unread projects make a separate row map.
		if next.bytes <= d.o.Bytes {
			next.compactRows()
		}
	} else {
		// next may carry missing auxiliary data, but shares the compact row map.
		next.affixes = base.affixes
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire()
	if old == nil || cursor == nil || d.held[*cursor] != old {
		base.at = d.o.Now()
		return fullResponse(s, d.keep(base), cursor != nil), nil
	}
	d.drop(*cursor)
	next.at = d.o.Now()
	nextCursor := d.keep(next)
	if nextCursor == nil {
		// Do not carry unavailable data into a full response's baseline. Reuse
		// the original material even if only that smaller baseline fits.
		base.at = next.at
		return fullResponse(s, d.keep(base), true), nil
	}
	out["cursor"] = nextCursor
	return out, nil
}
