package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-intake/server/internal/giteareads"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
)

var assetRoute = regexp.MustCompile(`^/repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/issues/(?:[1-9][0-9]*|comments/[1-9][0-9]*)/assets$`)
var attachmentName = regexp.MustCompile(`^desk-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}-[a-f0-9]{64}\.(?:png|jpg)$`)

func (c *Client) UploadAttachment(ctx context.Context, route, name, mime string, data []byte) (map[string]any, error) {
	if !assetRoute.MatchString(route) || !attachmentName.MatchString(name) || mime != "image/png" && mime != "image/jpeg" || len(data) == 0 || len(data) > 2<<20 {
		return nil, errors.New("画像の転送先または内容が不正です")
	}
	body, contentType, e := attachmentForm(name, mime, data)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base.String(), "/")+"/api/v1"+route, body)
	if e != nil {
		return nil, errors.New("画像の転送を準備できません")
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "issue-desk-intake")
	response, e := c.http.Do(req)
	if e != nil {
		giteareads.Process.Record("POST", route, 0, true)
		return nil, &Error{UnknownOutcome: true}
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	failed := e != nil || len(raw) > 8<<20 || response.StatusCode < 200 || response.StatusCode >= 300
	giteareads.Process.Record("POST", route, len(raw), failed)
	if e != nil || len(raw) > 8<<20 {
		return nil, &Error{Status: response.StatusCode, UnknownOutcome: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &Error{Status: response.StatusCode, UnknownOutcome: response.StatusCode >= 500 || response.StatusCode < 400}
	}
	out := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&out) != nil || out == nil {
		return nil, &Error{Status: response.StatusCode, UnknownOutcome: true}
	}
	return out, nil
}
func (c *Client) CheckAttachmentSupport(ctx context.Context) error {
	req, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.base.String(), "/")+"/swagger.v1.json", nil)
	if e != nil {
		return errors.New("添付API仕様を確認できません")
	}
	response, e := c.http.Do(req)
	if e != nil {
		return errors.New("添付API仕様を確認できません")
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if e != nil || len(raw) > 8<<20 || response.StatusCode != 200 {
		return errors.New("添付API仕様を確認できません")
	}
	var spec struct {
		Paths map[string]struct {
			Get  json.RawMessage `json:"get"`
			Post struct {
				Parameters []struct{ In, Name, Type string } `json:"parameters"`
			} `json:"post"`
		} `json:"paths"`
	}
	if json.Unmarshal(raw, &spec) != nil {
		return errors.New("添付API仕様が不正です")
	}
	for _, route := range []string{"/repos/{owner}/{repo}/issues/{index}/assets", "/repos/{owner}/{repo}/issues/comments/{id}/assets"} {
		path, exists := spec.Paths[route]
		supported := false
		for _, p := range path.Post.Parameters {
			supported = supported || p.In == "formData" && p.Name == "attachment" && p.Type == "file"
		}
		if !exists || len(path.Get) == 0 || string(path.Get) == "null" || !supported {
			return errors.New("このGiteaは画像の添付APIに対応していません")
		}
	}
	return nil
}
func (c *Client) AttachmentLink(uuid string) string {
	if !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`).MatchString(uuid) {
		return ""
	}
	base := c.base
	if c.publicURL != nil {
		base = c.publicURL
	}
	return strings.TrimRight(base.String(), "/") + "/attachments/" + uuid
}

// WithPublicURL returns a configured copy; response URLs never choose the origin.
func (c *Client) WithPublicURL(value string) (*Client, error) {
	if strings.TrimSpace(value) == "" {
		copy := *c
		copy.publicURL = nil
		return &copy, nil
	}
	u, e := url.Parse(value)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("Giteaの公開URLが不正です")
	}
	copy := *c
	copy.publicURL = u
	return &copy, nil
}

// attachmentForm encodes the image as the multipart form Gitea expects.
func attachmentForm(name, mime string, data []byte) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="attachment"; filename="`+name+`"`)
	header.Set("Content-Type", mime)
	part, e := writer.CreatePart(header)
	if e != nil {
		return nil, "", errors.New("画像の転送を準備できません")
	}
	if _, e = part.Write(data); e != nil {
		return nil, "", e
	}
	if e = writer.Close(); e != nil {
		return nil, "", e
	}
	return &body, writer.FormDataContentType(), nil
}
