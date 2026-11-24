package hostdrift

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"strings"
	"time"
)

func (s *Service) release(ctx context.Context, host, current string) (*hostd.Release, map[string]time.Time, *int, error) {
	release, e := s.desiredRelease(ctx, host, current)
	if e != nil {
		return nil, nil, nil, e
	}
	if release == nil {
		return nil, nil, nil, nil
	}
	times := map[string]time.Time{}
	if strings.EqualFold(release.Revision, current) {
		count := 0
		return release, times, &count, nil
	}
	history, e := s.releaseHistory(ctx, release)
	if e != nil {
		return nil, nil, nil, e
	}
	times, count := releaseChain(history, release, current)
	return release, times, count, nil
}

// desiredRelease はPCが取るべき版の登録を読む。種類が分からなければ -python の登録も探す。
func (s *Service) desiredRelease(ctx context.Context, host, current string) (*hostd.Release, error) {
	var flavor string
	e := s.DB.QueryRowContext(ctx, "SELECT flavor FROM host_image_results WHERE host=$1 AND left(tag,12)=$2 AND status='success' ORDER BY at DESC LIMIT 1", host, current[:12]).Scan(&flavor)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	knownFlavor := e == nil
	service := hostd.Service{DB: s.DB, Now: s.Now}
	read := func(flavor string) (*hostd.Release, error) {
		raw, e := service.Desired(ctx, host, flavor, current)
		if e != nil {
			return nil, e
		}
		value := fleet.Object(raw)["desired"]
		if value == nil {
			return nil, nil
		}
		r, ok := value.(*hostd.Release)
		if ok {
			return r, nil
		}
		data, e := json.Marshal(value)
		if e != nil {
			return nil, e
		}
		var result hostd.Release
		e = json.Unmarshal(data, &result)
		return &result, e
	}
	release, e := read(flavor)
	if e != nil {
		return nil, e
	}
	if release == nil && flavor == "" && !knownFlavor {
		release, e = read("-python")
		if e != nil {
			return nil, e
		}
	}
	return release, nil
}

// releaseHistoryEntry は発行履歴の1件。
type releaseHistoryEntry struct {
	base, kind string
	at         time.Time
}

// releaseHistory は版の登録先の発行履歴を revision ごとに読む。
func (s *Service) releaseHistory(ctx context.Context, release *hostd.Release) (map[string]releaseHistoryEntry, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT revision,base_revision,kind,created_at FROM client_release_history WHERE host=$1 AND flavor=$2", release.Host, release.Flavor)
	if e != nil {
		return nil, e
	}
	history := map[string]releaseHistoryEntry{}
	for rows.Next() {
		var revision, kind string
		var base sql.NullString
		var at time.Time
		if e = rows.Scan(&revision, &base, &kind, &at); e != nil {
			rows.Close()
			return nil, e
		}
		history[strings.ToLower(revision)] = releaseHistoryEntry{strings.ToLower(base.String), kind, at}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	return history, nil
}

// releaseChain は発行済みの版から今の版まで発行履歴をたどり、更新の種類ごとの最も古い発行時刻と隔たりの数を返す。たどれなければ nil を返す。
func releaseChain(history map[string]releaseHistoryEntry, release *hostd.Release, current string) (map[string]time.Time, *int) {
	times := map[string]time.Time{}
	revision := strings.ToLower(release.Revision)
	visited := map[string]bool{}
	for revision != "" && revision != current && !visited[revision] {
		visited[revision] = true
		entry, ok := history[revision]
		if !ok || (entry.kind != "align" && entry.kind != "soon" && entry.kind != "lazy") || entry.at.UnixMilli() <= 0 {
			return nil, nil
		}
		if at := times[entry.kind]; at.IsZero() || entry.at.Before(at) {
			times[entry.kind] = entry.at
		}
		revision = entry.base
	}
	if revision != current {
		return nil, nil
	}
	count := len(visited)
	return times, &count
}
