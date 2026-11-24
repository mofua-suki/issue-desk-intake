package screenapi

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"time"
)

var rowTypes = []string{"issue", "draft", "todo", "closeRequest"}

func (s *Service) snapshotConfig(ctx context.Context, origin string, timing *listTiming, config Object) (Snapshot, error) {
	started := time.Now()
	var waiting time.Duration
	defer func() {
		elapsed := time.Since(started)
		timing.snapshot += elapsed
		timing.stages["snapshot_prepare"] += elapsed - waiting
	}()
	for _, kind := range rowTypes {
		if kind == "todo" && ListScope(ctx) == "active" {
			continue
		}
		if s.o.Lists[kind] == nil {
			return Snapshot{}, fail(503, "一覧の取得元が設定されていません。")
		}
	}
	names := append(append([]string{}, auxiliary...), "hosts")
	rowsRead, auxRead, unread, waited, err := s.readSnapshotSources(ctx, origin, timing, names)
	waiting = waited
	if err != nil {
		return Snapshot{}, err
	}
	return buildSnapshot(config, rowsRead, auxRead, names, unread)
}

type snapshotRowResult struct {
	items    []Object
	err      error
	duration time.Duration
}
type snapshotAuxResult struct {
	value    any
	err      error
	duration time.Duration
	present  bool
}

// readSnapshotSources は一覧の各取得元と補助の情報を並行に読み、読み取りの時間を記録する。最初に失敗した取得元のエラーを優先して返す。
func (s *Service) readSnapshotSources(ctx context.Context, origin string, timing *listTiming, names []string) (rowsRead []snapshotRowResult, auxRead []snapshotAuxResult, unread *unreadCollector, waiting time.Duration, err error) {
	rowsRead = make([]snapshotRowResult, len(rowTypes))
	auxRead = make([]snapshotAuxResult, len(names))
	unread = &unreadCollector{items: map[string]string{}}
	readCtx, cancel := context.WithCancel(context.WithValue(ctx, unreadKey{}, unread))
	defer cancel()
	var wg sync.WaitGroup
	var failureMu sync.Mutex
	var firstFailure error
	for i, kind := range rowTypes {
		if kind == "todo" && ListScope(ctx) == "active" {
			continue
		}
		wg.Add(1)
		go func(i int, read ListReader) {
			defer wg.Done()
			start := time.Now()
			items, err := read(readCtx, origin)
			rowsRead[i] = snapshotRowResult{items, err, time.Since(start)}
			if err != nil {
				failureMu.Lock()
				if readCtx.Err() == nil {
					firstFailure = err
				}
				cancel()
				failureMu.Unlock()
			}
		}(i, s.o.Lists[kind])
	}
	for i, name := range names {
		if read := s.o.Aux[name]; read != nil {
			auxCtx := readCtx
			if name == "attentionAlerts" {
				auxCtx = context.WithValue(readCtx, attentionTimingKey{}, func(stage string, duration time.Duration) {
					for _, allowed := range attentionTimingNames {
						if stage == allowed && duration >= 0 {
							timing.stages["attentionAlerts_"+stage] += duration
							return
						}
					}
				})
			}
			wg.Add(1)
			go func(i int, read func(context.Context) (any, error), auxCtx context.Context) {
				defer wg.Done()
				start := time.Now()
				value, err := read(auxCtx)
				auxRead[i] = snapshotAuxResult{value, err, time.Since(start), true}
			}(i, read, auxCtx)
		}
	}
	waitStart := time.Now()
	wg.Wait()
	waiting = time.Since(waitStart)
	for i, kind := range rowTypes {
		timing.stages[kind+"_read"] += rowsRead[i].duration
	}
	for i, name := range names {
		if auxRead[i].present {
			timing.stages[name+"_read"] += auxRead[i].duration
		}
	}
	// The failure that initiated cancellation takes precedence over later driver errors.
	if firstFailure != nil {
		return rowsRead, auxRead, unread, waiting, firstFailure
	}
	if ctx.Err() != nil {
		return rowsRead, auxRead, unread, waiting, ctx.Err()
	}
	for _, result := range rowsRead {
		if result.err != nil {
			return rowsRead, auxRead, unread, waiting, result.err
		}
	}
	return rowsRead, auxRead, unread, waiting, nil
}

// buildSnapshot は読んだ行を見せてよいものに絞って種類ごとに並べ、補助の情報と合わせて一覧の材料にする。
func buildSnapshot(config Object, rowsRead []snapshotRowResult, auxRead []snapshotAuxResult, names []string, unread *unreadCollector) (Snapshot, error) {
	snapshot := Snapshot{Rows: []Row{}, Aux: map[string]any{}, UnreadProjects: unread.list()}
	if len(snapshot.UnreadProjects) == 0 {
		snapshot.UnreadProjects = nil
	}
	for i, kind := range rowTypes {
		items := rowsRead[i].items
		rows := []Row{}
		for _, v := range items {
			if !permitted(config, kind, v) {
				continue
			}
			if kind == "closeRequest" && !oneOf(fleet.Text(v["status"]), "reviewing", "confirm", "closing") {
				continue
			}
			item := Summarize(kind, v)
			key, e := rowKey(kind, item)
			if e != nil {
				return Snapshot{}, e
			}
			rows = append(rows, Row{key, kind, item})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].Item, rows[j].Item
			key := "createdAt"
			if kind == "issue" {
				key = "updatedAt"
			}
			aa, bb := fleet.Text(a[key]), fleet.Text(b[key])
			if aa == bb {
				return rows[i].Key < rows[j].Key
			}
			if kind == "closeRequest" {
				return aa < bb
			}
			return aa > bb
		})
		snapshot.Rows = append(snapshot.Rows, rows...)
	}
	for i, name := range names {
		result := auxRead[i]
		if !result.present || result.err != nil {
			continue
		}
		if name == "hosts" {
			snapshot.Hosts = result.value
		} else {
			snapshot.Aux[name] = result.value
		}
	}
	return snapshot, nil
}
func (s *Service) entry(ctx context.Context, q url.Values, origin string) (Object, error) {
	for key, values := range q {
		if !oneOf(key, "type", "project", "number", "id") || len(values) != 1 {
			return nil, fail(400, "一覧1件のクエリが不正です。")
		}
	}
	kind := q.Get("type")
	if !oneOf(kind, rowTypes...) {
		return nil, fail(400, "typeを指定してください。")
	}
	ident := Object{}
	if kind == "issue" {
		n, ok := safeNumber(q.Get("number"))
		if _, has := q["id"]; has || !ok || !repositoryPattern.MatchString(q.Get("project")) {
			return nil, fail(400, "issueはprojectとnumberだけ指定してください。")
		}
		ident["project"], ident["number"] = q.Get("project"), n
	} else {
		if q.Has("project") || q.Has("number") || !uuid.MatchString(q.Get("id")) {
			return nil, fail(400, "一覧項目はidだけ指定してください。")
		}
		ident["id"] = q.Get("id")
	}
	read := s.o.Entries[kind]
	if read == nil {
		return nil, fail(503, "一覧項目の取得元が設定されていません。")
	}
	item, e := read(ctx, ident, origin)
	if e != nil {
		return nil, e
	}
	if item == nil {
		return nil, fail(404, "一覧項目が見つかりません。")
	}
	config, e := s.o.Config()
	if e != nil {
		return nil, e
	}
	if !permitted(config, kind, item) {
		return nil, fail(403, "許可対象のプロジェクトではありません。")
	}
	if kind == "closeRequest" && !oneOf(fleet.Text(item["status"]), "reviewing", "confirm", "closing") {
		return nil, fail(404, "このクローズ依頼は一覧対象ではありません。")
	}
	item = Summarize(kind, item)
	key, e := rowKey(kind, item)
	if e != nil {
		return nil, e
	}
	return Object{"key": key, "type": kind, "item": item}, nil
}
func oneOf(value string, values ...string) bool {
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
