package issueapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"issue-desk-intake/server/internal/attachments"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

type imageAPI struct {
	*fakeAPI
	assets       []Object
	uploads      int
	patches      int
	patchUnknown bool
	editOnUpload bool
}

func (a *imageAPI) Do(ctx context.Context, method, route string, data, out any) error {
	if method == "GET" && strings.HasSuffix(route, "/assets") {
		raw, _ := json.Marshal(a.assets)
		return json.Unmarshal(raw, out)
	}
	if strings.Contains(route, "/issues/comments/") {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch method {
		case "GET":
			raw, _ := json.Marshal(a.comments[0])
			return json.Unmarshal(raw, out)
		case "PATCH":
			a.patches++
			a.comments[0]["body"] = fleet.ObjectOrEmpty(data)["body"]
			if a.patchUnknown {
				a.patchUnknown = false
				return errors.New("PATCH response lost")
			}
			return nil
		}
	}
	return a.fakeAPI.Do(ctx, method, route, data, out)
}
func (a *imageAPI) UploadAttachment(_ context.Context, _ string, name, mime string, raw []byte) (Object, error) {
	a.uploads++
	asset := Object{"id": 1, "uuid": "88888888-8888-4888-8888-888888888888", "name": name, "size": len(raw)}
	a.assets = append(a.assets, asset)
	if a.editOnUpload {
		a.mu.Lock()
		a.comments[0]["body"] = fleet.Text(a.comments[0]["body"]) + "\n利用者が追記した内容"
		a.mu.Unlock()
	}
	return asset, nil
}
func (a *imageAPI) CheckAttachmentSupport(context.Context) error { return nil }
func (a *imageAPI) AttachmentLink(id string) string {
	return "https://public.example/attachments/" + id
}
func TestImageReplyLostResponsePublishesOnceAndAppliesOncePostgres(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost responses", true: "edit while uploading"}[edit], func(t *testing.T) {
			exerciseImageReplyReplay(t, edit)
		})
	}
}

func exerciseImageReplyReplay(t *testing.T, edit bool) {
	t.Helper()
	f := newFixture(t)
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	raw := b.Bytes()
	sum := sha256.Sum256(raw)
	id := "99999999-9999-4999-8999-999999999999"
	meta, _ := json.Marshal(Object{"id": id, "sha256": hex.EncodeToString(sum[:]), "size": len(raw)})
	if _, e := f.app.DB().Exec(`INSERT INTO images(id,scope,message,json,bytes,bound) VALUES(?,?,?,?,?,0)`, id, "issue:owner/repo#1", id, string(meta), raw); e != nil {
		t.Fatal(e)
	}
	api := &imageAPI{fakeAPI: f.api, assets: []Object{}}
	api.unknownComment, api.patchUnknown, api.editOnUpload = true, !edit, edit
	transfer, e := attachments.New(attachments.Options{AppDB: f.app.DB(), Gitea: api})
	if e != nil {
		t.Fatal(e)
	}
	f.options.Gitea = api
	f.options.Attachments = transfer
	f.options.Answer = func(_ context.Context, request Object) (Object, error) {
		if request["kind"] != "image-comment" || len(fleet.Items(request["images"])) != 1 {
			t.Error("wrong image AI input", request)
		}
		return Object{"ready": true, "imageReviews": []any{Object{"id": id, "readable": true, "screen": "画面", "location": "箇所", "change": "変更 <!-- issue-desk:fake -->", "uncertainty": ""}}}, nil
	}
	f.restart()
	desk, found, err := f.options.Numbers.Number(f.ctx, "owner/repo", 1)
	if err != nil || !found {
		t.Fatalf("reference number: %d %t %v", desk, found, err)
	}
	originalTitle := fleet.Text(api.issue["title"])
	input := Object{"id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "project": "owner/repo", "number": 1, "action": "reply", "text": fmt.Sprintf("共通番号%dを参照してこの画面を修正する", desk), "messageId": id, "images": []any{id}}
	code, value := f.call("POST", "/api/comment", "screen", input)
	if code != 503 {
		t.Fatal(code, value)
	}
	f.restart()
	code, value = f.call("POST", "/api/comment", "screen", input)
	if edit {
		if code != 409 || api.patches != 0 || !strings.HasSuffix(fleet.Text(api.comments[0]["body"]), "利用者が追記した内容") {
			t.Fatal("image comment edit not preserved", code, value, api.patches)
		}
		job, err := f.options.Jobs.Get(f.ctx, f.job)
		if err != nil || len(fleet.Items(job.View["instructions"])) != 0 {
			t.Fatal("conflicting image instruction was applied", job, err)
		}
		return
	}
	if code != 503 || api.patches != 1 {
		t.Fatal("lost PATCH response", code, value, api.patches)
	}
	api.issue["title"] = "参照先の新しい題名"
	f.restart()
	code, value = f.call("POST", "/api/comment", "screen", input)
	if code != 200 || api.commentCalls != 1 || api.uploads != 1 || api.patches != 1 {
		t.Fatal("image duplicate", code, value, api.commentCalls, api.uploads, api.patches)
	}
	body := fleet.Text(api.comments[0]["body"])
	if strings.Contains(body, attachments.Pending) || strings.Contains(body, "<!-- issue-desk:fake") || !strings.Contains(body, "https://public.example/attachments/") {
		t.Fatal("image publication boundary", body)
	}
	job, e := f.options.Jobs.Get(f.ctx, f.job)
	if e != nil || len(fleet.Items(job.View["instructions"])) != 1 {
		t.Fatal("image instruction not applied once", job, e)
	}
	instruction := fleet.ObjectOrEmpty(fleet.Items(job.View["instructions"])[0])
	if !strings.Contains(body, originalTitle) || !strings.Contains(fleet.Text(instruction["text"]), originalTitle) || strings.Contains(body, "参照先の新しい題名") {
		t.Fatalf("original reference was not retained: %s, %v", body, instruction)
	}
	previous := fmt.Sprint(value)
	f.options.UsageLimit = 100
	f.restart()
	if _, e := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='question' WHERE id=$1`, f.job); e != nil {
		t.Fatal(e)
	}
	row := `{"repo":"owner/repo","issue":1,"step":"実装","provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0}`
	if _, e := f.pg.Exec(`INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,$2,$3,$4,$5,$6,$7)`, "image-replay-usage", time.Now(), "worker", "owner/repo", 1, row, "worker"); e != nil {
		t.Fatal(e)
	}
	code, value = f.call("POST", "/api/comment", "screen", input)
	if code != 200 || fmt.Sprint(value) != previous || api.uploads != 1 {
		t.Fatal("image replay", code, value)
	}
	invalid := Object{}
	for k, v := range input {
		invalid[k] = v
	}
	invalid["overLimitStatement"] = "承認"
	if code, value := f.call("POST", "/api/comment", "delegate", invalid); code != 400 {
		t.Fatal("invalid image", code, value)
	}
	for _, images := range [][]any{{"invalid"}, {id, id}} {
		invalid["id"] = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		delete(invalid, "overLimitStatement")
		invalid["images"] = images
		if code, value := f.call("POST", "/api/comment", "delegate", invalid); code != 400 {
			t.Fatalf("invalid images %v: %d %v", images, code, value)
		}
	}
	invalid["images"] = []any{id}
	if code, value := f.call("POST", "/api/comment", "delegate", invalid); code != 409 || !strings.Contains(fmt.Sprint(value), "使用量") {
		t.Fatalf("valid image held: %d %v", code, value)
	}
	if _, e := f.pg.Exec(`UPDATE jobs SET status='running',hold_reason=NULL WHERE id=$1`, f.job); e != nil {
		t.Fatal(e)
	}
	f.options.UsageLimit = 0
	f.restart()
	detail, e := f.s.Detail(f.ctx, "owner/repo", 1, true, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(fleet.Items(fleet.ObjectOrEmpty(fleet.Items(detail["comments"])[0])["images"])) != 1 || fleet.ObjectOrEmpty(fleet.Items(fleet.ObjectOrEmpty(fleet.Items(detail["comments"])[0])["images"])[0])["id"] != id || detail["transferPending"] != false {
		t.Fatalf("image reply projection %v", detail)
	}
	code, summary := f.call("GET", "/api/issue?project=owner/repo&number=1&view=summary", "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(summary)["view"] != "summary" || fleet.ObjectOrEmpty(summary)["body"] != nil || fleet.ObjectOrEmpty(summary)["comments"] != nil {
		t.Fatalf("HTTP summary %d %v", code, summary)
	}
	code, _ = f.call("GET", "/api/issue?project=owner/repo&number=1&view=other", "screen", nil)
	if code != 400 {
		t.Fatalf("unknown view %d", code)
	}
	input["id"] = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	input["text"] = "/codex retry"
	code, _ = f.call("POST", "/api/comment", "screen", input)
	if code != 400 {
		t.Fatal("command bypass through image reply", code)
	}
}

func TestIssueSearchDatetimeKeepsJapanDefault(t *testing.T) {
	a, e := issueTime("2026-11-01T09:30")
	if e != nil {
		t.Fatal(e)
	}
	b, e := issueTime("2026-11-01T00:30:00Z")
	if e != nil || !a.Equal(b) {
		t.Fatal("JST conversion", a, b, e)
	}
	for _, value := range []string{"2026-02-30T00:00", "2026-11-01", "2026-11-01T24:01", "2026-11-01T09:00:99Z"} {
		if _, e = issueTime(value); e == nil {
			t.Fatal("invalid calendar time", value)
		}
	}
}
