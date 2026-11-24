package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"testing"
	"time"
)

type blockedReplyAPI struct {
	API
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockedReplyAPI) Issue(ctx context.Context, repo string, n int64) (Object, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return b.API.Issue(ctx, repo, n)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestAsyncReplyReturnsIDBeforeGiteaAndReconcilesOnce(t *testing.T) {
	f := newFixture(t)
	b := &blockedReplyAPI{API: f.api, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.o.Gitea = b
	var release sync.Once
	defer release.Do(func() { close(b.release) })
	id := "11111111-1111-4111-8111-111111111113"
	data := Object{"id": id, "project": "owner/repo", "number": 1, "action": "reply", "text": "追加の指示", "async": true}
	code, value := f.call("POST", "/api/comment", "screen", data)
	if code != 202 || fleet.ObjectOrEmpty(value)["accepted"] != true || fleet.ObjectOrEmpty(value)["id"] != id {
		t.Fatalf("%d %v", code, value)
	}
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("返信が開始しない")
	}
	code, value = f.call("GET", "/api/comment?id="+id, "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(value)["status"] != "queued" {
		t.Fatalf("待機: %d %v", code, value)
	}
	// Replays must acknowledge without waiting for the in-flight external call.
	code, value = f.call("POST", "/api/comment", "screen", data)
	if code != 202 || fleet.ObjectOrEmpty(value)["id"] != id {
		t.Fatalf("再送: %d %v", code, value)
	}
	changed := clone(data)
	changed["text"] = "異なる指示"
	code, _ = f.call("POST", "/api/comment", "screen", changed)
	if code != 409 {
		t.Fatalf("異なる本文を受理: %d", code)
	}
	release.Do(func() { close(b.release) })
	if err := f.s.WaitAccepted(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, value = f.call("GET", "/api/comment?id="+id, "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(value)["status"] != "done" || fleet.ObjectOrEmpty(value)["result"] == nil || f.api.commentCalls != 1 {
		t.Fatalf("完了: %d %v %d", code, value, f.api.commentCalls)
	}
	if err := f.s.recoverReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WaitAccepted(context.Background()); err != nil || f.api.commentCalls != 1 {
		t.Fatalf("復旧で再投稿: %v %d", err, f.api.commentCalls)
	}
}
func TestAsyncReplyRecoveryRevalidatesDelegation(t *testing.T) {
	f := newFixture(t)
	id := "11111111-1111-4111-8111-111111111114"
	queued := Object{"id": id, "owner": "token:revoked", "via": "token:nas", "input": Object{"id": id, "project": "owner/repo", "number": 1, "action": "reply", "text": "指示"}, "status": "queued"}
	if err := f.s.save(f.ctx, "actions", replyPrefix+id, queued); err != nil {
		t.Fatal(err)
	}
	f.s.o.Principal = func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{}, fail(401, "失効済み")
	}
	if err := f.s.recoverReplies(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WaitAccepted(f.ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := f.s.read(f.ctx, "actions", replyPrefix+id)
	if err != nil || saved["status"] != "failed" || saved["error"] != "失効済み" || f.api.commentCalls != 0 {
		t.Fatalf("%v %v", saved, err)
	}
	if _, err = f.s.replyRequest(f.ctx, id, auth.Principal{Kind: auth.PC, TokenID: "other", Delegated: true}); err == nil {
		t.Fatal("別の送り手が結果を読めた")
	}
}

func TestAsyncReplyRecoversUnknownCommentOutcomeWithSameID(t *testing.T) {
	f := newFixture(t)
	id := "11111111-1111-4111-8111-111111111117"
	f.api.unknownComment = true
	data := Object{"id": id, "project": "owner/repo", "number": 1, "action": "reply", "text": "追加の指示"}
	if _, err := f.s.acceptReply(f.ctx, data, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WaitAccepted(f.ctx); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.read(f.ctx, "actions", replyPrefix+id)
	if err != nil || r["status"] != "queued" || f.api.commentCalls != 1 {
		t.Fatalf("結果不明: %v %v posts=%d", r, err, f.api.commentCalls)
	}
	posted := f.api.comments
	f.api.comments = nil // A temporarily incomplete Gitea history must not open a resend path.
	if err := f.s.recoverReplies(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WaitAccepted(f.ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := f.s.read(f.ctx, "actions", replyPrefix+id)
	if err != nil || pending["status"] != "queued" || f.api.commentCalls != 1 {
		t.Fatalf("照合中の409を確定拒否にした: %v %v", pending, err)
	}
	f.api.comments = posted
	if err := f.s.recoverReplies(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.WaitAccepted(f.ctx); err != nil {
		t.Fatal(err)
	}
	r, err = f.s.read(f.ctx, "actions", replyPrefix+id)
	if err != nil || r["status"] != "done" || r["error"] != nil || f.api.commentCalls != 1 {
		t.Fatalf("同IDの照合: %v %v posts=%d", r, err, f.api.commentCalls)
	}
}
