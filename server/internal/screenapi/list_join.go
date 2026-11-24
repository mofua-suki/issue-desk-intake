// 同じ条件の一覧の読み取りを1回にまとめ、待っている要求へ同じ結果を配る。

package screenapi

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
)

// A running read serves only its original arrivals. Later arrivals form the next batch.
type listJoinKey struct{ origin, scope, repositories string }

type listBatch struct {
	done     chan struct{}
	next     *listBatch
	waiters  int
	snapshot Snapshot
	err      error
	timing   *listTiming
}

type listJoins struct {
	mu      sync.Mutex
	running map[listJoinKey]*listBatch
}

func (s *Service) joinSnapshot(ctx context.Context, origin, scope string, config Object, timing *listTiming) (Snapshot, error) {
	// JSON encoding is deterministic for map keys; failure must not merge distinct policies.
	policy, err := json.Marshal(config["repositories"])
	if err != nil {
		return Snapshot{}, err
	}
	key := listJoinKey{origin, scope, string(policy)}
	j := &s.joins
	j.mu.Lock()
	if j.running == nil {
		j.running = make(map[listJoinKey]*listBatch)
	}
	batch := j.running[key]
	if batch == nil {
		batch = &listBatch{done: make(chan struct{}), timing: newListTiming()}
		j.running[key] = batch
		go s.runListBatch(context.WithoutCancel(ctx), key, config, batch)
	} else {
		if batch.next == nil {
			batch.next = &listBatch{done: make(chan struct{}), timing: newListTiming()}
		}
		batch = batch.next
	}
	batch.waiters++
	j.mu.Unlock()
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-batch.done:
	}
	if batch.err != nil {
		return Snapshot{}, batch.err
	}
	for name, duration := range batch.timing.stages {
		timing.stages[name] += duration
	}
	timing.snapshot += batch.timing.snapshot
	// Delta processing and responses must not share mutable maps or slices.
	data, err := json.Marshal(batch.snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	var result Snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	err = decoder.Decode(&result)
	return result, err
}

func (s *Service) runListBatch(ctx context.Context, key listJoinKey, config Object, batch *listBatch) {
	batch.snapshot, batch.err = s.snapshotConfig(ctx, key.origin, batch.timing, config)
	j := &s.joins
	j.mu.Lock()
	next := batch.next
	if next == nil {
		delete(j.running, key)
	} else {
		j.running[key] = next
		go s.runListBatch(ctx, key, config, next)
	}
	close(batch.done)
	j.mu.Unlock()
}
