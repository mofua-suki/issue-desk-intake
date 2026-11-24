package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureClient(t *testing.T, f transport) *Client {
	t.Helper()
	u, _ := url.Parse("https://gitea.example")
	return newClient(u, "fixture-secret", f)
}
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type interruptedBody struct{}

func (interruptedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (interruptedBody) Close() error             { return nil }

func TestDialRefusalAndResponseInterruption(t *testing.T) {
	var c *Client
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		address := listener.Addr().String()
		listener.Close()
		base, _ := url.Parse("http://" + address)
		c = newClient(base, "fixture-secret", &http.Transport{Proxy: nil})
	} else {
		c = fixtureClient(t, func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		})
	}
	err = c.Do(context.Background(), "POST", "/issues", map[string]any{}, nil)
	var failure *Error
	if !errors.As(err, &failure) || !failure.DialRefused || !failure.UnknownOutcome {
		t.Fatalf("dial refusal: %v", err)
	}
	cases := []struct {
		name      string
		transport transport
	}{
		{"response interrupted", func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }},
		{"body read interrupted", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 201, Header: make(http.Header), Body: interruptedBody{}}, nil
		}},
		{"non-dial refusal", func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNREFUSED}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fixtureClient(t, tc.transport).Do(context.Background(), "POST", "/issues", map[string]any{}, nil)
			var failure *Error
			if !errors.As(err, &failure) || !failure.UnknownOutcome || failure.DialRefused {
				t.Fatalf("interruption: %v", err)
			}
		})
	}
}

func TestMutationFailureDoesNotRetryOrExposeRequestSecrets(t *testing.T) {
	count := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		count++
		if r.Header.Get("Authorization") != "token fixture-secret" {
			t.Fatal("missing authentication")
		}
		return nil, errors.New("network failure containing fixture-secret")
	})
	err := c.Do(context.Background(), "POST", "/repos/owner/app/issues", map[string]any{"title": "request"}, &map[string]any{})
	var failure *Error
	if !errors.As(err, &failure) || !failure.UnknownOutcome || count != 1 || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("unsafe mutation handling: %v %d", err, count)
	}
}

func TestMutationWithUnreadableSuccessIsStillAnUnknownOutcome(t *testing.T) {
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) { return response(201, "not-json"), nil })
	err := c.Do(context.Background(), "POST", "/repos/owner/app/issues", map[string]any{}, &map[string]any{})
	var failure *Error
	if !errors.As(err, &failure) || !failure.UnknownOutcome {
		t.Fatalf("successful mutation may be retried: %v", err)
	}
}

func TestRedirectCannotReceiveToken(t *testing.T) {
	count := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.Host != "gitea.example" {
			t.Fatal("secret sent to redirect")
		}
		res := response(302, "")
		res.Header.Set("Location", "https://attacker.example")
		return res, nil
	})
	if err := c.Do(context.Background(), "GET", "/user", nil, &map[string]any{}); err == nil || count != 1 {
		t.Fatalf("redirect followed: %v %d", err, count)
	}
}

func TestPaginationFetchesAllPagesAndRetainsJSONIntegers(t *testing.T) {
	count := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.Query().Get("page") == "1" {
			return response(200, "["+strings.TrimSuffix(strings.Repeat(`{"id":9007199254740991},`, 50), ",")+"]"), nil
		}
		return response(200, `[{"id":42}]`), nil
	})
	rows, err := c.Pages(context.Background(), "/repos/owner/app/issues?state=open")
	if err != nil || len(rows) != 51 || count != 2 {
		t.Fatalf("partial pagination: %d %d %v", len(rows), count, err)
	}
	if got := rows[0]["id"].(json.Number).String(); got != "9007199254740991" {
		t.Fatal("integer precision lost")
	}
}

func TestFileReadingIsBoundedAndUsesPinnedRevision(t *testing.T) {
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("ref") != "pinned-sha" {
			t.Fatal("unfixed reference")
		}
		return response(200, `{"type":"file","encoding":"base64","size":3,"content":"YWJj"}`), nil
	})
	if got, err := c.File(context.Background(), "owner/app", "pinned-sha", "docs/a.md", 3); err != nil || string(got) != "abc" {
		t.Fatalf("file: %q %v", got, err)
	}
	if _, err := c.File(context.Background(), "owner/app", "pinned-sha", "docs/a.md", 2); err == nil {
		t.Fatal("file exceeded budget")
	}
	if _, err := c.File(context.Background(), "owner/app", "pinned-sha", "../secret", 3); err == nil {
		t.Fatal("unsafe reference path")
	}
}

func TestCredentialFileMustBePrivate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("fixture-secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New("https://gitea.example", file, nil); err == nil {
		t.Fatal("public credential accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New("https://gitea.example", file, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCommentsEndpointReturnsCompleteUnpaginatedCollection(t *testing.T) {
	calls := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.RawQuery != "" {
			t.Fatal("unsupported comments pagination")
		}
		return response(200, "["+strings.TrimSuffix(strings.Repeat(`{"id":1},`, 150), ",")+"]"), nil
	})
	comments, err := c.Comments(context.Background(), "owner/app", 1)
	if err != nil || calls != 1 || len(comments) != 150 {
		t.Fatalf("%d %d %v", calls, len(comments), err)
	}
}

func TestRepository423Classification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		status      int
		unreachable bool
		archived    bool
		unknown     bool
		calls       int
	}{
		{name: "archived", body: `{"archived":true,"default_branch":"main"}`, status: 200, archived: true, calls: 1},
		{name: "active", body: `{"archived":false}`, status: 200, calls: 1},
		{name: "missing", body: `{"default_branch":"main"}`, status: 200, unknown: true, calls: 1},
		{name: "null", body: `{"archived":null}`, status: 200, unknown: true, calls: 1},
		{name: "string", body: `{"archived":"false"}`, status: 200, unknown: true, calls: 1},
		{name: "number", body: `{"archived":1}`, status: 200, unknown: true, calls: 1},
		{name: "object", body: `{"archived":{}}`, status: 200, unknown: true, calls: 1},
		{name: "array flag", body: `{"archived":[]}`, status: 200, unknown: true, calls: 1},
		{name: "null repository", body: `null`, status: 200, unknown: true, calls: 1},
		{name: "array repository", body: `[]`, status: 200, unknown: true, calls: 1},
		{name: "invalid JSON", body: `{"archived":`, status: 200, unknown: true, calls: 1},
		{name: "empty response", status: 200, unknown: true, calls: 1},
		{name: "no content", status: 204, unknown: true, calls: 1},
		{name: "locked lookup", body: `{"archived":true}`, status: 423, unknown: true, calls: 1},
		{name: "forbidden", status: 403, unknown: true, calls: 1},
		{name: "not found", status: 404, unknown: true, calls: 1},
		{name: "server failure", status: 503, unknown: true, calls: 3},
		{name: "network failure", unreachable: true, unknown: true, calls: 3},
		{name: "response limit", body: `{"archived":true,"padding":"` + strings.Repeat("x", 8<<20) + `"}`, status: 200, unknown: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.String() != "https://gitea.example/api/v1/repos/owner/app" || r.Header.Get("Authorization") != "token fixture-secret" {
					t.Fatal("repository lookup did not use authenticated repository GET")
				}
				if tc.unreachable {
					return nil, errors.New("network failure containing fixture-secret")
				}
				return response(tc.status, tc.body), nil
			})
			archived, err := c.RepositoryArchived(context.Background(), "owner/app")
			if (err != nil) != tc.unknown || archived != tc.archived || calls != tc.calls {
				t.Fatalf("archived=%v err=%v calls=%d; want archived=%v unknown=%v calls=%d", archived, err, calls, tc.archived, tc.unknown, tc.calls)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("lookup exposed credentials")
			}
			var failure *Error
			if errors.As(err, &failure) && failure.UnknownOutcome {
				t.Fatal("read failure classified as an uncertain mutation")
			}
		})
	}
}

func TestRepositoryArchivedRejectsInvalidTarget(t *testing.T) {
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid target reached transport")
		return nil, errors.New("unexpected request")
	})
	for _, repo := range []string{"", "owner", "owner/app/extra", "../app", "owner/..", "owner/app?other", "owner/app#other", "owner/app\\other", "owner/app\nother", "https://attacker.example/app"} {
		if archived, err := c.RepositoryArchived(context.Background(), repo); err == nil || archived {
			t.Fatalf("invalid repository %q: archived=%v err=%v", repo, archived, err)
		}
	}
}
