package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/pgtest"
	"strings"
	"testing"
)

type fake struct {
	assets       []Object
	uploads      int
	unknown      bool
	body         string
	version      int
	patches      int
	patchUnknown bool
}

func (f *fake) Do(_ context.Context, method, route string, data, out any) error {
	if method == "GET" {
		var value any = f.assets
		if !strings.HasSuffix(route, "/assets") {
			value = Object{"body": f.body, "content_version": f.version}
		}
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	}
	if method == "PATCH" {
		fields := fleet.ObjectOrEmpty(data)
		if fleet.Int64(fields["content_version"]) != int64(f.version) {
			return fail("content version mismatch")
		}
		f.patches++
		f.version++
		f.body = fleet.Text(fields["body"])
		if f.patchUnknown {
			f.patchUnknown = false
			return &gitea.Error{UnknownOutcome: true}
		}
		return nil
	}
	return fail("unexpected fixture")
}
func (f *fake) UploadAttachment(_ context.Context, _ string, name, mime string, data []byte) (Object, error) {
	f.uploads++
	asset := Object{"id": float64(1), "uuid": "22222222-2222-4222-8222-222222222222", "name": name, "size": len(data)}
	f.assets = append(f.assets, asset)
	if f.unknown {
		f.unknown = false
		return nil, &gitea.Error{UnknownOutcome: true}
	}
	return asset, nil
}
func (f *fake) CheckAttachmentSupport(context.Context) error { return nil }
func (f *fake) AttachmentLink(id string) string              { return "https://gitea.example/attachments/" + id }
func TestTransferUnknownResultUsesPersistedImageReceipt(t *testing.T) {
	ctx := context.Background()
	f := &fake{assets: []Object{}, unknown: true}
	s := &Service{o: Options{Gitea: f}}
	im := Image{ID: "11111111-1111-4111-8111-111111111111", SHA256: strings.Repeat("a", 64), Ext: "png", MIME: "image/png", Size: 5, Bytes: []byte("image")}
	state := Object{}
	saves := 0
	save := func() error {
		saves++
		if saves == 1 && fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(state["uploads"])[im.ID])["attempted"] != true {
			t.Fatal("attempt not saved before upload")
		}
		return nil
	}
	if _, e := s.Transfer(ctx, "/repos/owner/repo/issues/1/assets", []Image{im}, state, save); e == nil {
		t.Fatal("lost upload not surfaced")
	}
	links, e := s.Transfer(ctx, "/repos/owner/repo/issues/1/assets", []Image{im}, state, save)
	if e != nil || f.uploads != 1 || !strings.HasPrefix(links[im.ID], "https://gitea.example/") {
		t.Fatal("upload not reconciled", f.uploads, links, e)
	}
	f.assets = []Object{}
	if _, e = s.Transfer(ctx, "/repos/owner/repo/issues/1/assets", []Image{im}, state, save); e == nil || f.uploads != 1 {
		t.Fatal("unknown missing asset uploaded again")
	}
}
func TestAttachKeepsScopeReviewsAndPendingGate(t *testing.T) {
	ctx := context.Background()
	previousDialect := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previousDialect) })
	db := pgtest.New(t)
	var e error
	var b bytes.Buffer
	if e = png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 1, 1))); e != nil {
		t.Fatal(e)
	}
	raw := b.Bytes()
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	id := "11111111-1111-4111-8111-111111111111"
	meta, _ := json.Marshal(Object{"id": id, "sha256": hash, "size": len(raw)})
	if _, e = db.Exec(`INSERT INTO images(id,scope,message,json,bytes,bound) VALUES(?,?,?,?,?,0)`, id, "draft:"+id, id, string(meta), raw); e != nil {
		t.Fatal(e)
	}
	f := &fake{assets: []Object{}, body: "依頼本文\n" + Pending, version: 1, patchUnknown: true}
	s, e := New(Options{AppDB: db, Gitea: f})
	if e != nil {
		t.Fatal(e)
	}
	draft := map[string]any{"id": id, "project": "owner/repo", "postBody": "依頼本文\n" + Pending, "messages": []any{Object{"images": []any{Object{"id": id}}}}, "imageReviews": []any{Object{"id": id, "readable": true, "screen": "画面", "location": "箇所", "change": "変更 <!-- issue-desk:fake -->", "uncertainty": ""}}, "untouched": "保持"}
	encoded, _ := json.Marshal(draft)
	if _, e = db.Exec(`INSERT INTO drafts(id,json) VALUES(?,?)`, id, string(encoded)); e != nil {
		t.Fatal(e)
	}
	issue := Object{"number": float64(1)}
	if e = s.Attach(ctx, draft, issue); e == nil {
		t.Fatal("lost PATCH response not surfaced")
	}
	if e = s.Attach(ctx, draft, issue); e != nil || f.patches != 1 {
		t.Fatalf("PATCH was not reconciled: patches=%d err=%v", f.patches, e)
	}
	if strings.Contains(f.body, Pending) || strings.Contains(f.body, "<!-- issue-desk:fake") || !strings.Contains(f.body, "&lt;!-- issue-desk:fake") {
		t.Fatal("publication marker boundary", f.body)
	}
	f.body += "\n利用者の追記"
	f.version++
	if e = s.Attach(ctx, draft, issue); e == nil || f.patches != 1 || !strings.HasSuffix(f.body, "利用者の追記") {
		t.Fatalf("issue edit was not preserved: %q patches=%d err=%v", f.body, f.patches, e)
	}
	var record string
	if e = db.QueryRow(`SELECT json FROM drafts WHERE id=?`, id).Scan(&record); e != nil {
		t.Fatal(e)
	}
	stored := Object{}
	_ = json.Unmarshal([]byte(record), &stored)
	if stored["untouched"] != "保持" || stored["attachmentsPublished"] != true || fleet.ObjectOrEmpty(stored["uploads"])[id] == nil {
		t.Fatal("receipt lost existing fields", stored)
	}
	if _, e = s.Selected(ctx, "draft:33333333-3333-4333-8333-333333333333", []string{id}, ""); e == nil {
		t.Fatal("different scope selected")
	}
	if _, err := s.Selected(ctx, "draft:"+id, []string{"33333333-3333-4333-8333-333333333333"}, ""); err == nil || err.Error() != "画像が見つかりません。" {
		t.Fatalf("missing attachment: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Selected(cancelled, "draft:"+id, []string{id}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled attachment read: %v", err)
	}
}

func TestNewRequiresApplicationDB(t *testing.T) {
	if service, err := New(Options{AppDB: &sql.DB{}, Gitea: &fake{}}); err != nil || service == nil {
		t.Fatalf("database unexpectedly rejected: service=%v err=%v", service, err)
	}
	if service, err := New(Options{Gitea: &fake{}}); err == nil || service != nil {
		t.Fatalf("nil database unexpectedly accepted: service=%v err=%v", service, err)
	}
}
