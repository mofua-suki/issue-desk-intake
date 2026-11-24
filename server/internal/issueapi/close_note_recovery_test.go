package issueapi

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

type recoveryReadCounter struct {
	API
	reads atomic.Int32
}

func (c *recoveryReadCounter) Issue(ctx context.Context, r string, n int64) (Object, error) {
	c.reads.Add(1)
	return c.API.Issue(ctx, r, n)
}
func (c *recoveryReadCounter) Comments(ctx context.Context, r string, n int64) ([]Object, error) {
	c.reads.Add(1)
	return c.API.Comments(ctx, r, n)
}
func TestLegacyClosedRecoveryDoesNotReadOrRepostCompletedNotes(t *testing.T) {
	f := newFixture(t)
	tx, e := f.app.DB().Begin()
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 200; i++ {
		record := Object{"id": fmt.Sprintf("%08d-1111-4111-8111-111111111111", i), "status": "closed", "project": "owner/repo", "number": 1, "comment": "保存済みの補足", "notePending": false}
		if i%2 == 0 {
			delete(record, "notePending")
		}
		raw, _ := json.Marshal(record)
		if _, e = tx.Exec("INSERT INTO close_requests(id,json)VALUES(?,?)", record["id"], string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	counter := &recoveryReadCounter{API: f.api}
	f.s.o.Gitea = counter
	ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
	defer cancel()
	if e = f.s.Run(ctx); e != nil {
		t.Fatal(e)
	}
	if counter.reads.Load() != 0 || f.api.commentCalls != 0 {
		t.Fatal("送信済み旧記録を背景で再読/再投稿しました", counter.reads.Load(), f.api.commentCalls)
	}
}
func TestPendingLegacyCloseNoteRecognizesOwnMarkerWithoutBodyEquality(t *testing.T) {
	f := newFixture(t)
	record := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "closed", "project": "owner/repo", "number": 1, "comment": "補足", "notePending": true}
	f.api.comments = []Object{{"id": float64(1), "user": Object{"login": "intake"}, "body": "旧正本の判断理由・説明を含む本文\n" + closeNoteMarker(record)}}
	if e := f.s.postCloseNote(f.ctx, record); e != nil {
		t.Fatal(e)
	}
	if record["notePending"] != false || f.api.commentCalls != 0 {
		t.Fatal(record, f.api.commentCalls)
	}
	if e := f.s.postCloseNote(f.ctx, record); e != nil {
		t.Fatal(e)
	}
	if f.api.commentCalls != 0 {
		t.Fatal("同じ記録を再投稿しました")
	}
}
func TestPendingCloseNotePostsOnceAndDoesNotTrustForeignMarker(t *testing.T) {
	f := newFixture(t)
	record := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "closed", "project": "owner/repo", "number": 1, "notePending": true, "reason": "voluntary", "description": "本人の明示操作"}
	f.api.comments = []Object{{"id": float64(1), "user": Object{"login": "foreign"}, "body": closeNoteMarker(record)}}
	for i := 0; i < 2; i++ {
		if e := f.s.postCloseNote(f.ctx, record); e != nil {
			t.Fatal(e)
		}
	}
	if f.api.commentCalls != 1 || record["notePending"] != false || record["notePosted"] != true {
		t.Fatal(record, f.api.commentCalls)
	}
}

type blockedCloseRead struct {
	API
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (c *blockedCloseRead) Issue(ctx context.Context, repo string, n int64) (Object, error) {
	if c.calls.Add(1) == 1 {
		close(c.entered)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
	}
	return c.API.Issue(ctx, repo, n)
}
func TestCloseRecoveryDoesNotQueueDuplicateBackgroundWork(t *testing.T) {
	f := newFixture(t)
	f.api.issue["state"] = "closed"
	f.api.issue["closed_at"] = "2026-11-01T00:00:00Z"
	record := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "closed", "project": "owner/repo", "number": 1, "notePending": true}
	f.api.comments = []Object{{"id": float64(1), "user": Object{"login": "intake"}, "body": closeNoteMarker(record)}}
	if e := f.s.saveClose(f.ctx, record); e != nil {
		t.Fatal(e)
	}
	blocked := &blockedCloseRead{API: f.api, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.o.Gitea = blocked
	f.s.launchClose(fleet.Text(record["id"]))
	<-blocked.entered
	for i := 0; i < 100; i++ {
		f.s.launchClose(fleet.Text(record["id"]))
	}
	close(blocked.release)
	f.s.wg.Wait()
	if blocked.calls.Load() != 1 || f.api.commentCalls != 0 {
		t.Fatal("同じ対象の背景処理が重複しました", blocked.calls.Load(), f.api.commentCalls)
	}
}

func TestExplicitCloseNotePreservesCaller(t *testing.T) {
	body := closeNoteBody(Object{"id": "fixture", "owner": "token:pc", "reason": "voluntary", "decision": Object{"method": "explicit", "via": "token:pc"}})
	if strings.Contains(body, "受付画面") || !strings.Contains(body, "利用者の明示指示（PC別トークン pc）") {
		t.Fatal(body)
	}
}
