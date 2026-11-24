package adminapi

// Frozen sequential notifyPhone from c1d1b9b8225b62185b704b5ca75e998a07d58ec7.
// Source push.go SHA-256: ecdc8753220ee5c1b1545d04093c9d5bc93b5b2cfbca01778f1940e6cc9ede3e.
// Keep this test-only baseline unchanged so both modes use the same crypto,
// validation, state persistence and HTTP client behavior in comparisons.
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	webpush "github.com/SherClockHolmes/webpush-go"
	"issue-desk-intake/server/internal/contract"
)

func (s *service) notifyPhoneSequentialBaseline(ctx context.Context, notice PhoneNotice) PhonePushResult {
	result := PhonePushResult{}
	if contract.UTF16Length(notice.Title) > 100 || contract.UTF16Length(notice.Body) > 300 || !strings.HasPrefix(notice.URL, "/") || strings.HasPrefix(notice.URL, "//") || strings.ContainsAny(notice.URL, "\\?") || strings.ContainsFunc(notice.URL, unicode.IsSpace) {
		result.Failed = 1
		return result
	}
	parsed, e := url.Parse(notice.URL)
	if e != nil || parsed.IsAbs() {
		result.Failed = 1
		return result
	}
	var private, public string
	list := []any{}
	e = s.withPush(func(state map[string]any) (bool, error) {
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
	client := http.Client{Timeout: 10 * time.Second}
	if s.HTTPClient != nil {
		client = *s.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	for _, value := range list {
		v := obj(value)
		valid, e := subscription(v)
		if e != nil {
			result.Failed++
			continue
		}
		keys := obj(valid["keys"])
		item := &webpush.Subscription{Endpoint: str(valid["endpoint"]), Keys: webpush.Keys{P256dh: str(keys["p256dh"]), Auth: str(keys["auth"])}}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		response, e := webpush.SendNotificationWithContext(requestCtx, payload, item, &webpush.Options{HTTPClient: &client, TTL: 300, Subscriber: "mailto:issue-desk@example.invalid", VAPIDPublicKey: public, VAPIDPrivateKey: private, VapidExpiration: s.Now().Add(12 * time.Hour)})
		cancel()
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				response.Body.Close()
			}
		}
		if e != nil || status < 200 || status >= 300 {
			result.Failed++
			s.log("Web Pushの送信に失敗しました。" + pushFailure(status, e))
			if status == 404 || status == 410 {
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
		} else {
			result.Sent++
		}
	}
	return result
}
