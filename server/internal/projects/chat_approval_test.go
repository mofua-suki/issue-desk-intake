package projects

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tokenCall sends path with a PC token instead of the screen session (the chat AI's route).
func (f *fixture) tokenCall(method, path, token string, input any) (int, Object) {
	f.t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(string(raw)))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	out := Object{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (f *fixture) addToken(id, name, token string, delegated bool) {
	f.t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, e := f.db.AddPCToken(ctx, store.PCTokenRecord{ID: id, Name: name, Hash: auth.Hash(token), IssuedAt: now}); e != nil {
		f.t.Fatal(e)
	}
	if delegated {
		if _, e := f.db.SetDelegated(ctx, id, true, now); e != nil {
			f.t.Fatal(e)
		}
	}
}

// 新しいプロジェクトの作成への同意は、画面のほかに、運用の代行を許したトークンが利用者のチャットの承認の発言を添えて送れる。
// 発言の無い・定型語だけの同意と、代行の無いトークンは断り、作成を始めない。
func TestProjectChatApprovalNeedsDelegationAndUserStatement(t *testing.T) {
	f := newFixture(t)
	if code, v := f.call("/api/projects", Object{"id": id, "text": "プロジェクトを作る"}); code != 202 {
		t.Fatalf("create %d %v", code, v)
	}
	p := f.wait("ready")
	plain := "idk_" + strings.Repeat("a", 43)
	delegate := "idk_" + strings.Repeat("b", 43)
	f.addToken("t1", "plain-pc", plain, false)
	f.addToken("t2", "nas", delegate, true)
	if code, _ := f.tokenCall("GET", "/api/projects/"+id, plain, nil); code != 403 {
		t.Fatal("代行の無いトークンが相談を読めた", code)
	}
	if code, read := f.tokenCall("GET", "/api/projects/"+id, delegate, nil); code != 200 || read["fingerprint"] != p["fingerprint"] {
		t.Fatal("代行トークンで定義の指紋を読めない", code, read)
	}
	for _, c := range []struct {
		token string
		body  Object
		code  int
	}{
		{plain, Object{"fingerprint": p["fingerprint"], "statement": "共通番号1のリポジトリ作成に同意します"}, 403},
		{delegate, Object{"fingerprint": p["fingerprint"]}, 400},
		{delegate, Object{"fingerprint": p["fingerprint"], "statement": "どうぞ よろしくお願いいたします。", "instructionAt": "2026-11-01T08:58:00+09:00"}, 400},
		{delegate, Object{"fingerprint": p["fingerprint"], "statement": "共通番号1のリポジトリ作成に同意します"}, 400},
		{delegate, Object{"fingerprint": p["fingerprint"], "statement": "共通番号1のリポジトリ作成に同意します", "instructionAt": "2026-11-01T09:01:00+09:00"}, 400},
	} {
		if code, v := f.tokenCall("POST", "/api/projects/"+id+"/approve", c.token, c.body); code != c.code {
			t.Fatalf("want %d got %d %v", c.code, code, v)
		}
	}
	if code, _ := f.tokenCall("POST", "/api/projects/"+id+"/chat", delegate, Object{"text": "名前を変える"}); code != 403 {
		t.Fatal("同意以外の操作をトークンから受け付けた", code)
	}
	if f.git.createCalls != 0 {
		t.Fatal("同意の前に作成した")
	}
	statement := "質問「プロジェクト demo のリポジトリを作成しますか」への利用者の回答: A"
	if code, v := f.tokenCall("POST", "/api/projects/"+id+"/approve", delegate, Object{"fingerprint": p["fingerprint"], "statement": statement, "instructionAt": "2026-11-01T08:58:00+09:00"}); code != 200 {
		t.Fatalf("approve %d %v", code, v)
	}
	p = f.wait("registered")
	approval := fleet.ObjectOrEmpty(p["approval"])
	if approval["via"] != "chat" || approval["pc"] != "nas" || approval["statement"] != statement || approval["instructionAt"] != "2026-11-01T08:58:00+09:00" || f.git.createCalls != 1 {
		t.Fatal("チャットの同意を記録して作成していない", approval, f.git.createCalls)
	}
}
