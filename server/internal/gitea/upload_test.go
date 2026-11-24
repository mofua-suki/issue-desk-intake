package gitea

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAttachmentUploadBoundsMultipartAndUnknownOutcome(t *testing.T) {
	calls := 0
	c := fixtureClient(t, transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v1/repos/owner/repo/issues/1/assets" || r.Header.Get("Authorization") != "token fixture-secret" {
			t.Fatalf("boundary %s %v", r.URL, r.Header)
		}
		reader, e := r.MultipartReader()
		if e != nil {
			t.Fatal(e)
		}
		part, e := reader.NextPart()
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := io.ReadAll(part)
		if part.FormName() != "attachment" || !strings.HasPrefix(part.FileName(), "desk-") || part.Header.Get("Content-Type") != "image/png" || string(raw) != "image" {
			t.Fatal("multipart mismatch")
		}
		return response(201, `{"name":"saved"}`), nil
	}))
	name := "desk-11111111-1111-4111-8111-111111111111-" + strings.Repeat("a", 64) + ".png"
	if _, e := c.UploadAttachment(context.Background(), "/repos/owner/repo/issues/1/assets", name, "image/png", []byte("image")); e != nil {
		t.Fatal(e)
	}
	for _, route := range []string{"https://attacker.invalid/assets", "//attacker.invalid/assets", "/repos/owner/repo/issues/../assets"} {
		if _, e := c.UploadAttachment(context.Background(), route, name, "image/png", []byte("image")); e == nil {
			t.Fatal("external target accepted")
		}
	}
	if _, e := c.UploadAttachment(context.Background(), "/repos/owner/repo/issues/1/assets", name, "image/png", make([]byte, (2<<20)+1)); e == nil {
		t.Fatal("oversized upload accepted")
	}
	if calls != 1 {
		t.Fatal("invalid upload reached transport", calls)
	}
	c = fixtureClient(t, transport(func(*http.Request) (*http.Response, error) { return response(201, `{`), nil }))
	if _, e := c.UploadAttachment(context.Background(), "/repos/owner/repo/issues/1/assets", name, "image/png", []byte("image")); e == nil {
		t.Fatal("unknown result accepted")
	} else if g, ok := e.(*Error); !ok || !g.UnknownOutcome {
		t.Fatal("unknown upload not fenced", e)
	}
}
func TestConfiguredPublicAttachmentOrigin(t *testing.T) {
	c := fixtureClient(t, transport(func(*http.Request) (*http.Response, error) {
		t.Fatal("URL設定だけで接続しました")
		return nil, nil
	}))
	public, e := c.WithPublicURL("https://public.example:8443")
	if e != nil {
		t.Fatal(e)
	}
	id := "11111111-1111-4111-8111-111111111111"
	if public.AttachmentLink(id) != "https://public.example:8443/attachments/"+id || c.AttachmentLink(id) != "https://gitea.example/attachments/"+id {
		t.Fatal("公開URLまたは元Clientが変わりました")
	}
	for _, v := range []string{"https://user:secret@public.example", "https://public.example?x=1", "https://public.example?", "https://public.example#part", "//public.example", "file:///tmp/image", "https://public.example/path"} {
		if _, e = c.WithPublicURL(v); e == nil {
			t.Fatal("invalid public origin", v)
		}
	}
}
