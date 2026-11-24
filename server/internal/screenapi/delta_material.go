package screenapi

import (
	"sort"
	"strings"
)

// prepareBaseline marshals each present value once. bytes always accounts for
// the original UTF-8 JSON, independently of the lossless retained representation.
func prepareBaseline(scope string, s Snapshot) (*baseline, error) {
	b := &baseline{scope: scope, rows: make(map[string]string, len(s.Rows)), aux: map[string]string{}}
	for _, r := range s.Rows {
		if _, exists := b.rows[r.Key]; r.Key == "" || exists {
			return nil, fail(503, "一覧の行の識別子を確認できません。")
		}
		sig := signature(r.Item)
		b.rows[r.Key] = sig
		b.bytes += len(r.Key) + len(sig)
	}
	for _, name := range auxiliary {
		if value, has := s.Aux[name]; has {
			sig := signature(value)
			b.aux[name] = sig
			b.bytes += len(sig)
		}
	}
	if s.Hosts != nil {
		sig := hostSignature(s.Hosts)
		b.hosts = &sig
		b.bytes += len(sig)
	}
	return b, nil
}

type rowAffix struct{ prefix, suffix string }

// Group by the kind in the row key so the four shipped row schemas can each
// share their exact JSON prefix/suffix. Keys without a colon remain valid too.
func rowGroup(key string) string {
	group, _, _ := strings.Cut(key, ":")
	return group
}

// compactRows factors common bytes without hashes or lossy equality. Clone is
// essential: substrings must not retain the original JSON backing storage.
// Publication happens afterwards; registered baselines are immutable.
func (b *baseline) compactRows() {
	type bounds struct {
		first                 string
		prefix, suffix, count int
	}
	groups := map[string]*bounds{}
	for key, sig := range b.rows {
		group := rowGroup(key)
		v := groups[group]
		if v == nil {
			groups[group] = &bounds{sig, len(sig), len(sig), 1}
			continue
		}
		v.count++
		if v.prefix > len(sig) {
			v.prefix = len(sig)
		}
		for i := 0; i < v.prefix; i++ {
			if v.first[i] != sig[i] {
				v.prefix = i
				break
			}
		}
		if v.suffix > len(sig) {
			v.suffix = len(sig)
		}
		for i := 0; i < v.suffix; i++ {
			if v.first[len(v.first)-1-i] != sig[len(sig)-1-i] {
				v.suffix = i
				break
			}
		}
	}
	// Prevent overlapping prefix/suffix, including identical or empty JSON.
	for key, sig := range b.rows {
		v := groups[rowGroup(key)]
		if v.suffix > len(sig)-v.prefix {
			v.suffix = len(sig) - v.prefix
		}
	}
	b.affixes = map[string]rowAffix{}
	for group, v := range groups {
		// Avoid copying rows for savings too small to offset allocation rounding.
		if v.count < 2 || v.prefix+v.suffix < 32 {
			continue
		}
		b.affixes[group] = rowAffix{strings.Clone(v.first[:v.prefix]), strings.Clone(v.first[len(v.first)-v.suffix:])}
	}
	for key, sig := range b.rows {
		if a, ok := b.affixes[rowGroup(key)]; ok {
			b.rows[key] = strings.Clone(sig[len(a.prefix) : len(sig)-len(a.suffix)])
		}
	}
}

func (b *baseline) rowEqual(key, before, current string) bool {
	a := b.affixes[rowGroup(key)]
	return len(current) == len(a.prefix)+len(before)+len(a.suffix) &&
		strings.HasPrefix(current, a.prefix) && strings.HasSuffix(current, a.suffix) &&
		current[len(a.prefix):len(current)-len(a.suffix)] == before
}

// compareBaseline never modifies old. Missing aux/hosts are carried into a
// separate candidate so a later full fallback can still register base as-is.
func compareBaseline(old, base *baseline, s Snapshot) (Object, *baseline) {
	added, updated := []Row{}, []Row{}
	removed := []string{}
	for _, r := range s.Rows {
		before, exists := old.rows[r.Key]
		if !exists {
			added = append(added, r)
		} else if !old.rowEqual(r.Key, before, base.rows[r.Key]) {
			updated = append(updated, r)
		}
	}
	// Rows of a project that could not be read are absent, not deleted: keep
	// them in the next baseline instead of reporting them removed.
	unread := unreadSet(s)
	carried := map[string]string{}
	for key, before := range old.rows {
		if _, exists := base.rows[key]; !exists {
			if project, ok := issueRowProject(key); ok && unread[project] {
				a := old.affixes[rowGroup(key)]
				carried[key] = a.prefix + before + a.suffix
				continue
			}
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	next := *base
	if len(carried) > 0 {
		next.rows = make(map[string]string, len(base.rows)+len(carried))
		for key, sig := range base.rows {
			next.rows[key] = sig
		}
		for key, sig := range carried {
			next.rows[key] = sig
			next.bytes += len(key) + len(sig)
		}
		next.ownRows = true
		next.affixes = nil
	}
	next.aux = make(map[string]string, len(base.aux))
	for name, sig := range base.aux {
		next.aux[name] = sig
	}
	aux := Object{}
	for _, name := range auxiliary {
		if value, has := s.Aux[name]; !has {
			if before, exists := old.aux[name]; exists {
				next.aux[name] = before
				next.bytes += len(before)
			}
		} else if old.aux[name] != base.aux[name] {
			aux[name] = value
		}
	}
	hostChanged := false
	if s.Hosts == nil {
		if old.hosts != nil {
			next.hosts = old.hosts
			next.bytes += len(*old.hosts)
		}
	} else {
		hostChanged = old.hosts == nil || *old.hosts != *base.hosts
	}
	out := Object{"full": false, "reset": false, "added": added, "updated": updated, "removed": removed, "aux": aux, "unavailable": unavailable(s)}
	if hostChanged {
		out["hosts"] = s.Hosts
	}
	if len(s.UnreadProjects) > 0 {
		out["unreadProjects"] = s.UnreadProjects
	}
	return out, &next
}
