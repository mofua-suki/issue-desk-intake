package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"issue-desk-intake/server/internal/giteareads"
)

type Error struct {
	Status         int
	UnknownOutcome bool
	DialRefused    bool
	Cause          error
}

func (e *Error) Error() string {
	if e.UnknownOutcome {
		return "Giteaへの送信結果を確認できません。同じ依頼を照合してください。"
	}
	if e.Status == 0 {
		return "Giteaへ接続できません。"
	}
	return fmt.Sprintf("GiteaがHTTP %dを返しました。", e.Status)
}
func (e *Error) Unwrap() error { return e.Cause }

type Client struct {
	history   *issueHistory
	base      *url.URL
	publicURL *url.URL
	token     string
	http      *http.Client
}

func New(base, tokenFile string, transport http.RoundTripper) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("Giteaの接続先が不正です")
	}
	f, err := os.Open(tokenFile)
	if err != nil {
		return nil, errors.New("Giteaの資格情報を開けません")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Giteaの資格情報は所有者だけが読める通常ファイルにしてください")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return nil, errors.New("Giteaの資格情報を読み取れません")
	}
	token := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("Giteaの資格情報が不正です")
	}
	return newClient(u, token, transport), nil
}

func newClient(base *url.URL, token string, transport http.RoundTripper) *Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{history: newIssueHistory(), base: base, token: token, http: &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) Do(ctx context.Context, method, endpoint string, payload any, out any) error {
	target, err := c.apiURL(endpoint)
	if err != nil {
		return err
	}
	var body []byte
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return errors.New("Giteaへの送信内容が不正です")
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := c.newRequest(ctx, method, target, body, payload != nil)
		if err != nil {
			return err
		}
		res, err := c.http.Do(req)
		if err != nil {
			giteareads.Process.Record(method, endpoint, 0, true)
			if ctx.Err() != nil {
				if method != http.MethodGet {
					return &Error{UnknownOutcome: true, Cause: ctx.Err()}
				}
				return ctx.Err()
			}
			if attempt+1 < attempts {
				if err := pause(ctx, time.Duration(1<<attempt)*500*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			var op *net.OpError
			refused := errors.As(err, &op) && op.Op == "dial" && errors.Is(op.Err, syscall.ECONNREFUSED)
			return &Error{UnknownOutcome: method != http.MethodGet, DialRefused: refused}
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
		res.Body.Close()
		giteareads.Process.Record(method, endpoint, len(data), readErr != nil || len(data) > 8<<20 || res.StatusCode < 200 || res.StatusCode >= 300)
		if readErr != nil || len(data) > 8<<20 {
			return &Error{UnknownOutcome: method != http.MethodGet}
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return decodeSuccess(method, res.StatusCode, data, out)
		}
		if (res.StatusCode == 429 || res.StatusCode >= 500) && attempt+1 < attempts {
			delay := time.Duration(1<<attempt) * 500 * time.Millisecond
			if value, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && value > 0 && value <= 30 {
				delay = time.Duration(value) * time.Second
			}
			if err := pause(ctx, delay); err != nil {
				return err
			}
			continue
		}
		return &Error{Status: res.StatusCode, UnknownOutcome: method != http.MethodGet && res.StatusCode >= 500}
	}
	return errors.New("Giteaの応答を確認できません")
}

// apiURL resolves the endpoint under the API of the configured server, rejecting
// a path that could leave it.
func (c *Client) apiURL(endpoint string) (*url.URL, error) {
	if !strings.HasPrefix(endpoint, "/") || strings.HasPrefix(endpoint, "//") || strings.ContainsAny(endpoint, "\\\x00\r\n") {
		return nil, errors.New("GiteaのAPIパスが不正です")
	}
	target, err := url.Parse(strings.TrimRight(c.base.String(), "/") + "/api/v1" + endpoint)
	if err != nil || target.Scheme != c.base.Scheme || target.Host != c.base.Host || target.User != nil {
		return nil, errors.New("GiteaのAPI接続先が不正です")
	}
	return target, nil
}

// newRequest builds an authenticated JSON request.
func (c *Client) newRequest(ctx context.Context, method string, target *url.URL, body []byte, hasPayload bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Giteaへの要求を作れません")
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "issue-desk-intake")
	if hasPayload {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// decodeSuccess decodes a successful response into out. A response that cannot
// be decoded leaves the outcome of a write unknown.
func decodeSuccess(method string, status int, data []byte, out any) error {
	if out == nil || status == 204 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		if method != http.MethodGet {
			return &Error{Status: status, UnknownOutcome: true}
		}
		return errors.New("Giteaの応答JSONを確認できません")
	}
	return nil
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func RepoPath(repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", errors.New("リポジトリ名が不正です")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n?#") {
			return "", errors.New("リポジトリ名が不正です")
		}
	}
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

func (c *Client) Pages(ctx context.Context, endpoint string) ([]map[string]any, error) {
	result := make([]map[string]any, 0)
	for page := 1; page <= 1000; page++ {
		separator := "?"
		if strings.Contains(endpoint, "?") {
			separator = "&"
		}
		var rows []map[string]any
		if err := c.Do(ctx, http.MethodGet, endpoint+separator+"limit=50&page="+strconv.Itoa(page), nil, &rows); err != nil {
			return nil, err
		}
		result = append(result, rows...)
		if len(rows) < 50 {
			return result, nil
		}
	}
	return nil, errors.New("Giteaの一覧を最後まで取得できませんでした")
}
