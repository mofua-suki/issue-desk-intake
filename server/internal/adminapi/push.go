package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"fmt"
	webpush "github.com/SherClockHolmes/webpush-go"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"unicode"
)

type PhoneNotice struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}
type PhonePushResult struct {
	Sent          int `json:"sent"`
	Failed        int `json:"failed"`
	Subscriptions int `json:"subscriptions"`
}

var pushPublic = regexp.MustCompile(`^[A-Za-z0-9_-]{87}$`)
var pushAuth = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
var authorizationSecret = regexp.MustCompile(`(?i)(Authorization:\s*\S+\s+)\S+`)

func redactAuthorization(value string) string {
	return authorizationSecret.ReplaceAllString(value, "${1}[REDACTED]")
}
func subscription(value map[string]any) (map[string]any, error) {
	endpoint := str(value["endpoint"])
	parsed, e := url.Parse(endpoint)
	keys := obj(value["keys"])
	if e != nil || parsed == nil || parsed.Scheme != "https" || !contains([]string{"web.push.apple.com", "fcm.googleapis.com", "updates.push.services.mozilla.com"}, parsed.Hostname()) || (parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil || parsed.Fragment != "" || len(endpoint) > 2048 || !pushPublic.MatchString(str(keys["p256dh"])) || !pushAuth.MatchString(str(keys["auth"])) {
		return nil, bad(400, "購読の形式が不正です。")
	}
	if parsed.Port() == "443" {
		parsed.Host = parsed.Hostname()
	}
	return map[string]any{"endpoint": parsed.String(), "keys": map[string]any{"p256dh": keys["p256dh"], "auth": keys["auth"]}}, nil
}
func (s *service) pushFile() (string, error) {
	root := s.RuntimeDir
	if root == "" {
		root = str(s.Config["runtimeDir"])
	}
	if root == "" {
		return "", bad(503, "通知の保存先が設定されていません。")
	}
	return filepath.Join(root, "state", "web-push.json"), nil
}
func (s *service) withPush(fn func(map[string]any) (bool, error)) error {
	file, e := s.pushFile()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(file), 0700); e != nil {
		return e
	}
	lock, e := os.OpenFile(file+".lock", os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state, initial, e := readPushState(file)
	if e != nil {
		return e
	}
	changed, e := fn(state)
	if e != nil {
		return e
	}
	if !initial && !changed {
		return os.Chmod(file, 0600)
	}
	return writePushState(file, state)
}

// readPushState reads the saved keys and subscriptions, or creates new keys
// when nothing is saved yet.
func readPushState(file string) (map[string]any, bool, error) {
	raw, e := os.ReadFile(file)
	state := map[string]any{}
	initial := errors.Is(e, os.ErrNotExist)
	if initial {
		private, public, e := webpush.GenerateVAPIDKeys()
		if e != nil {
			return nil, false, e
		}
		state["vapid"] = map[string]any{"publicKey": public, "privateKey": private}
		state["subscriptions"] = []any{}
	} else if e != nil {
		return nil, false, e
	} else if e = decodeJSON(raw, &state); e != nil {
		return nil, false, e
	}
	vapid := obj(state["vapid"])
	if str(vapid["publicKey"]) == "" || str(vapid["privateKey"]) == "" {
		return nil, false, bad(503, "通知の保存情報を読めません。")
	}
	if _, ok := state["subscriptions"].([]any); !ok {
		return nil, false, bad(503, "通知の保存情報を読めません。")
	}
	return state, initial, nil
}

// writePushState replaces the state file atomically and durably.
func writePushState(file string, state map[string]any) error {
	raw, e := json.Marshal(state)
	if e != nil {
		return e
	}
	temporary := file + "." + randomID() + ".tmp"
	f, e := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer os.Remove(temporary)
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(temporary, file); e != nil {
		return e
	}
	directory, e := os.Open(filepath.Dir(file))
	if e != nil {
		return e
	}
	defer directory.Close()
	return directory.Sync()
}
func (s *service) push(r *http.Request, _ auth.Principal) (any, int, error) {
	switch r.URL.Path {
	case "/api/push/key":
		if r.Method != http.MethodGet {
			return nil, 0, bad(405, "このメソッドは使えません。")
		}
		public := ""
		e := s.withPush(func(state map[string]any) (bool, error) {
			public = str(obj(state["vapid"])["publicKey"])
			return false, nil
		})
		if e != nil {
			return nil, 0, bad(503, "通知を準備できませんでした。")
		}
		return map[string]any{"publicKey": public}, 200, nil
	case "/api/push/subscriptions":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			return nil, 0, bad(405, "このメソッドは使えません。")
		}
		data, e := body(r)
		if e != nil {
			return nil, 0, e
		}
		endpoint := str(data["endpoint"])
		var replacement map[string]any
		if r.Method == http.MethodPost {
			replacement, e = subscription(data)
			if e != nil {
				return nil, 0, e
			}
			endpoint = str(replacement["endpoint"])
		} else if endpoint == "" {
			return nil, 0, bad(400, "購読の形式が不正です。")
		}
		if e = s.replaceSubscription(endpoint, replacement); e != nil {
			return nil, 0, bad(400, "購読を保存できませんでした。")
		}
		return map[string]any{"ok": true}, 200, nil
	case "/api/push/test":
		if r.Method != http.MethodPost {
			return nil, 0, bad(405, "このメソッドは使えません。")
		}
		s.notifyPhone(r.Context(), PhoneNotice{Title: "開発受付", Body: "テスト通知です。", URL: "/"})
		return map[string]any{"ok": true}, 200, nil
	}
	return nil, 0, bad(404, "経路が見つかりません。")
}

// NotifyPhone preserves the runtime state file and never exposes endpoint/key errors.
func NotifyPhone(ctx context.Context, runtimeDir string, notice PhoneNotice, client *http.Client) PhonePushResult {
	s := &service{Options: Options{RuntimeDir: runtimeDir, HTTPClient: client, Now: time.Now}}
	return s.notifyPhone(ctx, notice)
}
func (s *service) notifyPhone(ctx context.Context, notice PhoneNotice) PhonePushResult {
	result := PhonePushResult{}
	if !validPhoneNotice(notice) {
		result.Failed = 1
		return result
	}
	var private, public string
	list := []any{}
	e := s.withPush(func(state map[string]any) (bool, error) {
		vapid := obj(state["vapid"])
		private = str(vapid["privateKey"])
		public = str(vapid["publicKey"])
		list = append(list, state["subscriptions"].([]any)...)
		return false, nil
	})
	if e != nil {
		result.Failed = 1
		s.log("Web Pushの送信に失敗しました。" + pushFailure(0, e))
		return result
	}
	result.Subscriptions = len(list)
	payload, _ := json.Marshal(notice)
	client := s.phoneClient()
	// Only HTTP delivery runs concurrently. The caller collects results, logs and
	// rechecks expired subscriptions against the latest locked state.
	outcomes := make(chan phoneDelivery, 4)
	next, active := 0, 0
	for next < len(list) || active > 0 {
		for next < len(list) && active < cap(outcomes) {
			if ctx.Err() != nil {
				result.Failed += len(list) - next
				next = len(list)
				break
			}
			value := list[next]
			next++
			active++
			go func() {
				outcomes <- s.deliverPhone(ctx, value, payload, private, public, &client)
			}()
		}
		if active == 0 {
			break
		}
		delivery := <-outcomes
		active--
		if delivery.err == nil && delivery.status >= 200 && delivery.status < 300 {
			result.Sent++
			continue
		}
		result.Failed++
		if delivery.valid == nil {
			continue
		}
		s.log("Web Pushの送信に失敗しました。" + pushFailure(delivery.status, delivery.err))
		// A timeout/cancellation is never evidence that a subscription expired.
		if delivery.err == nil && (delivery.status == 404 || delivery.status == 410) {
			s.removeSubscription(delivery.valid)
		}
	}
	return result
}

// replaceSubscription removes the endpoint's subscription and saves the replacement, if any.
func (s *service) replaceSubscription(endpoint string, replacement map[string]any) error {
	return s.withPush(func(state map[string]any) (bool, error) {
		kept := []any{}
		for _, v := range state["subscriptions"].([]any) {
			if obj(v)["endpoint"] != endpoint {
				kept = append(kept, v)
			}
		}
		if replacement != nil {
			if len(kept) >= 32 {
				return false, bad(400, "通知を受け取る端末の上限です。")
			}
			kept = append(kept, replacement)
		}
		state["subscriptions"] = kept
		return true, nil
	})
}

// validPhoneNotice limits the title and body, and accepts only a relative path without a query.
func validPhoneNotice(notice PhoneNotice) bool {
	if contract.UTF16Length(notice.Title) > 100 || contract.UTF16Length(notice.Body) > 300 || !strings.HasPrefix(notice.URL, "/") || strings.HasPrefix(notice.URL, "//") || strings.ContainsAny(notice.URL, "\\?") || strings.ContainsFunc(notice.URL, unicode.IsSpace) {
		return false
	}
	parsed, e := url.Parse(notice.URL)
	return e == nil && !parsed.IsAbs()
}

// phoneClient is the delivery client: no redirects, at most 10 seconds.
func (s *service) phoneClient() http.Client {
	client := http.Client{Timeout: 10 * time.Second}
	if s.HTTPClient != nil {
		client = *s.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	return client
}

// removeSubscription removes the expired subscription when its endpoint and keys still match.
func (s *service) removeSubscription(valid map[string]any) {
	keys := obj(valid["keys"])
	_ = s.withPush(func(state map[string]any) (bool, error) {
		kept := []any{}
		for _, current := range state["subscriptions"].([]any) {
			m := obj(current)
			k := obj(m["keys"])
			if m["endpoint"] != valid["endpoint"] || k["auth"] != keys["auth"] || k["p256dh"] != keys["p256dh"] {
				kept = append(kept, current)
			}
		}
		state["subscriptions"] = kept
		return true, nil
	})
}

// pushFailure はログに残す原因を返す。購読先のURLは資格情報に当たるので、url.Error の操作先は残さない。
func pushFailure(status int, e error) string {
	var request *url.Error
	if errors.As(e, &request) {
		e = request.Err
	}
	if e != nil {
		return "原因: " + contract.SafeCause(e)
	}
	return fmt.Sprintf("HTTP %d", status)
}
