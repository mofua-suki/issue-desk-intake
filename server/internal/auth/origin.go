package auth

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

type RequestContext struct {
	Origin   string
	Local    bool
	Internal bool
}

func allowedOrigins(config Config) (map[string]string, error) {
	origins := map[string]string{}
	for _, host := range []string{"127.0.0.1", "localhost"} {
		hostPort := net.JoinHostPort(host, strconv.Itoa(config.Port))
		origins[hostPort] = "http://" + hostPort
	}
	if config.PublicURL != "" {
		u, err := url.Parse(config.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid public origin")
		}
		origins[u.Host] = u.Scheme + "://" + u.Host
	}
	if config.InternalHost != "" {
		u, err := url.Parse("http://" + config.InternalHost)
		if err != nil || u.Host != config.InternalHost || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid internal host")
		}
	}
	return origins, nil
}

func hasHeader(r *http.Request, key string) bool {
	for name := range r.Header {
		if strings.EqualFold(name, key) {
			return true
		}
	}
	return false
}

// Context trusts only configured Host values. Forwarding headers never confer
// local privileges, even when the request itself arrived over loopback.
func (a *Authenticator) Context(r *http.Request) (RequestContext, *contract.Problem) {
	if a.config.InternalHost != "" && r.Host == a.config.InternalHost {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") || !hasHeader(r, "Authorization") {
			return RequestContext{}, rejected(403, "内部の接続先ではAPIのトークン要求だけを受け付けます。")
		}
		return RequestContext{Origin: "http://" + a.config.InternalHost, Internal: true}, nil
	}
	origin, found := a.origins[r.Host]
	if !found {
		return RequestContext{}, rejected(403, "この接続先は許可されていません。")
	}
	local := loopbackPeer(r) && (strings.HasPrefix(origin, "http://127.0.0.1:") || strings.HasPrefix(origin, "http://localhost:"))
	return RequestContext{Origin: origin, Local: local}, nil
}

// loopbackPeer reports a direct loopback connection. Forwarding headers never
// confer local privileges, even when the request itself arrived over loopback.
func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return false
	}
	for name := range r.Header {
		if ForwardingHeader(name) || strings.HasPrefix(strings.ToLower(name), "tailscale-") {
			return false
		}
	}
	return true
}

// ForwardingHeader reports the proxy headers that mark a request as non-local.
func ForwardingHeader(name string) bool {
	lower := strings.ToLower(name)
	return lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-")
}

// LoopbackRequest applies the local-request rule of Context without a store, for
// the standby color that relays a request to the writer (startup.Relay).
func LoopbackRequest(r *http.Request, port int) bool {
	if port == 0 {
		port = 8787
	}
	p := strconv.Itoa(port)
	return (r.Host == net.JoinHostPort("127.0.0.1", p) || r.Host == net.JoinHostPort("localhost", p)) && loopbackPeer(r)
}
