package adminapi

import (
	"context"
	"io"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type phoneDelivery struct {
	valid  map[string]any
	status int
	err    error
}

// deliverPhone owns the request context and response body until delivery ends.
// It never writes state or invokes the application's log callback.
func (s *service) deliverPhone(ctx context.Context, value any, payload []byte, private, public string, client *http.Client) phoneDelivery {
	valid, err := subscription(obj(value))
	result := phoneDelivery{valid: valid, err: err}
	if err != nil {
		return result
	}
	keys := obj(valid["keys"])
	item := &webpush.Subscription{Endpoint: str(valid["endpoint"]), Keys: webpush.Keys{P256dh: str(keys["p256dh"]), Auth: str(keys["auth"])}}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Recheck after obtaining a delivery slot, including calls canceled while
	// this goroutine was waiting to run. The HTTP request also uses this context.
	if err = requestCtx.Err(); err != nil {
		result.err = err
		return result
	}
	response, err := webpush.SendNotificationWithContext(requestCtx, payload, item, &webpush.Options{HTTPClient: client, TTL: 300, Subscriber: "mailto:issue-desk@example.invalid", VAPIDPublicKey: public, VAPIDPrivateKey: private, VapidExpiration: s.Now().Add(12 * time.Hour)})
	result.err = err
	if response != nil {
		result.status = response.StatusCode
		if response.Body != nil {
			_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			if result.err == nil {
				result.err = readErr
			}
			response.Body.Close()
		}
	}
	if result.err == nil {
		result.err = requestCtx.Err()
	}
	return result
}
