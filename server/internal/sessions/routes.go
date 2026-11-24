package sessions

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

type Store interface {
	Session(context.Context, string) (auth.Session, bool, error)
	CreateSession(context.Context, string, auth.Session, time.Time) error
	ConsumeDesktopKey(context.Context, string, time.Time) (bool, error)
}

type Options struct {
	Auth      *auth.Authenticator
	Store     Store
	PublicURL string
	Now       func() time.Time
}

type Handler struct {
	options     Options
	mu          sync.Mutex
	pairHash    string
	pairExpires time.Time
	attempts    int
	resetAt     time.Time
}

func Register(mux *http.ServeMux, options Options) error {
	if options.Auth == nil || options.Store == nil {
		return errors.New("session persistence and authentication are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	h := &Handler{options: options}
	for _, p := range []string{"/api/session", "/api/desktop", "/api/pair", "/api/pair-code"} {
		mux.Handle(p, h)
	}
	return nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, message string) {
	contract.WriteProblem(w, contract.NewProblem(status, message, ""))
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, problem := h.options.Auth.Context(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if _, present := r.Header["Authorization"]; present {
		_, problem := h.options.Auth.Authenticate(r)
		if problem != nil {
			contract.WriteProblem(w, problem)
		} else {
			failure(w, 403, "この認証方法では操作できません。")
		}
		return
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodGet {
		h.session(w, r, ctx)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "このメソッドは利用できません。")
		return
	}
	if r.Header.Get("Origin") != ctx.Origin {
		failure(w, 403, "別の画面からの操作を拒否しました。")
		return
	}
	if r.URL.Path == "/api/pair-code" {
		h.pairCode(w, r, ctx)
		return
	}
	data, problem := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 128<<10)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	switch r.URL.Path {
	case "/api/desktop":
		key, ok := data["key"].(string)
		if !ctx.Local || !ok || key == "" {
			failure(w, 403, "PC用の起動リンクが無効です。「受付を開く」から開き直してください。")
			return
		}
		used, err := h.options.Store.ConsumeDesktopKey(r.Context(), auth.Hash(key), h.options.Now())
		if err != nil {
			failure(w, 503, "接続情報を保存できません。")
			return
		}
		if !used {
			failure(w, 403, "PC用の起動リンクが無効です。「受付を開く」から開き直してください。")
			return
		}
		h.create(w, r, ctx)
	case "/api/pair":
		h.pair(w, r, ctx, data)
	default:
		failure(w, 404, "操作が見つかりません。")
	}
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request, ctx auth.RequestContext) {
	value := map[string]any{"authenticated": false, "local": ctx.Local, "publicUrl": h.options.PublicURL}
	if cookie, err := r.Cookie("desk_session"); err == nil && cookie.Value != "" {
		session, found, err := h.options.Store.Session(r.Context(), auth.Hash(cookie.Value))
		if err != nil {
			failure(w, 503, "接続情報を読み取れません。")
			return
		}
		if found && session.ExpiresAt.After(h.options.Now()) {
			value["authenticated"], value["csrf"] = true, session.CSRF
		}
	}
	reply(w, 200, value)
}

func randomText(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, ctx auth.RequestContext) {
	key, err := randomText(32)
	if err != nil {
		failure(w, 503, "接続情報を作れません。")
		return
	}
	csrf, err := randomText(24)
	if err != nil {
		failure(w, 503, "接続情報を作れません。")
		return
	}
	now := h.options.Now()
	if err := h.options.Store.CreateSession(r.Context(), auth.Hash(key), auth.Session{CSRF: csrf, ExpiresAt: now.Add(30 * 24 * time.Hour)}, now); err != nil {
		failure(w, 503, "接続情報を保存できません。")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "desk_session", Value: key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 86400, Secure: strings.HasPrefix(ctx.Origin, "https:")})
	reply(w, 200, map[string]any{"authenticated": true, "csrf": csrf})
}

func (h *Handler) pairCode(w http.ResponseWriter, r *http.Request, ctx auth.RequestContext) {
	principal, problem := h.options.Auth.Authenticate(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if problem = auth.Authorize(principal, auth.Policy{Screen: true, CLI: true}); problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if !ctx.Local {
		failure(w, 403, "接続コードは受付と同じPCで発行してください。")
		return
	}
	value, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		failure(w, 503, "接続コードを作れません。")
		return
	}
	code := fmt.Sprintf("%08d", value)
	h.mu.Lock()
	h.pairHash = auth.Hash(code)
	h.pairExpires = h.options.Now().Add(10 * time.Minute)
	h.attempts = 0
	expires := h.pairExpires.UnixMilli()
	h.mu.Unlock()
	reply(w, 200, map[string]any{"code": code, "expires": expires})
}

func (h *Handler) pair(w http.ResponseWriter, r *http.Request, ctx auth.RequestContext, data map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.options.Now()
	if now.After(h.resetAt) {
		h.attempts = 0
		h.resetAt = now.Add(10 * time.Minute)
	}
	h.attempts++
	if h.attempts > 8 {
		failure(w, 429, "接続の試行回数が上限に達しました。10分後に再試行してください。")
		return
	}
	code, ok := data["code"].(string)
	if !ok || h.pairHash == "" || now.After(h.pairExpires) || subtle.ConstantTimeCompare([]byte(auth.Hash(strings.ReplaceAll(code, " ", ""))), []byte(h.pairHash)) != 1 {
		failure(w, 403, "接続コードが違うか、有効期限が切れています。")
		return
	}
	h.pairHash = ""
	h.create(w, r, ctx)
}
