// Package auth authenticates Intake request identities. Route handlers retain
// responsibility for project scope, resource ownership and state-based actions.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/contract"
)

type Kind string

const (
	PC     Kind = "pc-token"
	Hostd  Kind = "hostd-token"
	Screen Kind = "screen-session"
	CLI    Kind = "cli-key"
)

type Token struct {
	ID        string
	Name      string
	Delegated bool
}

type Session struct {
	CSRF      string
	ExpiresAt time.Time
}

// Store must use the authoritative token/session persistence. ConsumeCLIKey must
// atomically check expiry and delete exactly the consumed key; a read followed by
// an independent delete is unsafe. Backend errors fail closed with HTTP 503.
type Store interface {
	PCToken(context.Context, string) (Token, bool, error)
	TouchPCToken(context.Context, string, time.Time) error
	HostdToken(context.Context, string) (Token, bool, error)
	Session(context.Context, string) (Session, bool, error)
	ConsumeCLIKey(context.Context, string, time.Time) (bool, error)
}

type Principal struct {
	Kind      Kind
	Name      string
	TokenID   string
	Delegated bool
}

type Config struct {
	PublicURL    string
	Port         int
	InternalHost string
	Now          func() time.Time
}

type Authenticator struct {
	store   Store
	config  Config
	origins map[string]string
	// touched holds when each PC token's last_used_at was last written. The column is display only,
	// so a token used every few seconds is written at most once per touchInterval.
	touchMu sync.Mutex
	touched map[string]time.Time
}

// touchInterval bounds how often a PC token's last_used_at is rewritten.
const touchInterval = time.Minute

func New(config Config, store Store) (*Authenticator, error) {
	if store == nil {
		return nil, errors.New("authentication requires a persistent store")
	}
	if config.Port == 0 {
		config.Port = 8787
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("invalid authentication port")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	origins, err := allowedOrigins(config)
	if err != nil {
		return nil, err
	}
	return &Authenticator{store: store, config: config, origins: origins}, nil
}

func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

var tokenPattern = regexp.MustCompile(`^id[kh]_[A-Za-z0-9_-]{43}$`)
var relayKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// RelayKeyHeader carries the ticket a relaying color issues after it consumed a
// local one-time key (X-Desk-Key). RelayKeyHash keeps tickets apart from keys:
// a ticket is never accepted as X-Desk-Key and a key is never accepted as a ticket.
const RelayKeyHeader = "X-Issue-Desk-Relay-Key"

func RelayKeyHash(ticket string) string { return Hash("relay-key:" + ticket) }

var bearerPattern = regexp.MustCompile(`(?i)^Bearer\s+(\S+)$`)

// unavailable の応答は認証の保存先の失敗を外へ出さない。原因はログにだけ残す。
func unavailable(cause error) *contract.Problem {
	contract.LogFailure("認証情報の保存先", cause)
	return contract.NewProblem(503, "認証情報を読み書きできません。", "")
}

func rejected(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}

// Authenticate never falls back to another credential kind after Authorization
// is supplied, even if its content is empty, malformed, revoked or unavailable.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, *contract.Problem) {
	ctx, problem := a.Context(r)
	if problem != nil {
		return Principal{}, problem
	}
	if hasHeader(r, "Authorization") {
		if hasHeader(r, "Origin") && r.Header.Get("Origin") != ctx.Origin {
			return Principal{}, rejected(403, "別の画面からの操作を拒否しました。")
		}
		return a.bearer(r)
	}
	if r.Method != http.MethodGet && r.Header.Get("Origin") != ctx.Origin {
		return Principal{}, rejected(403, "別の画面からの操作を拒否しました。")
	}
	if cookie, err := r.Cookie("desk_session"); err == nil && cookie.Value != "" {
		session, found, err := a.store.Session(r.Context(), Hash(cookie.Value))
		if err != nil {
			return Principal{}, unavailable(err)
		}
		if found && session.ExpiresAt.After(a.config.Now()) {
			csrf := r.Header.Get("X-CSRF-Token")
			if r.Method != http.MethodGet && (session.CSRF == "" || subtle.ConstantTimeCompare([]byte(csrf), []byte(session.CSRF)) != 1) {
				return Principal{}, rejected(403, "画面を再読み込みしてください。")
			}
			return Principal{Kind: Screen}, nil
		}
	}
	if hasHeader(r, RelayKeyHeader) {
		// The other color consumed a local one-time key and vouches for it with a
		// ticket saved in the shared store. The ticket is single use.
		ticket := r.Header.Get(RelayKeyHeader)
		loopback := strings.HasPrefix(ctx.Origin, "http://127.0.0.1:") || strings.HasPrefix(ctx.Origin, "http://localhost:")
		if !loopback || !relayKeyPattern.MatchString(ticket) {
			return Principal{}, rejected(403, "PC用の一時キーが無効です。")
		}
		used, err := a.store.ConsumeCLIKey(r.Context(), RelayKeyHash(ticket), a.config.Now())
		if err != nil {
			return Principal{}, unavailable(err)
		}
		if !used {
			return Principal{}, rejected(403, "PC用の一時キーが無効です。")
		}
		return Principal{Kind: CLI}, nil
	}
	if ctx.Local && hasHeader(r, "X-Desk-Key") {
		key := r.Header.Get("X-Desk-Key")
		if key == "" {
			return Principal{}, rejected(403, "PC用の一時キーが無効です。")
		}
		used, err := a.store.ConsumeCLIKey(r.Context(), Hash(key), a.config.Now())
		if err != nil {
			return Principal{}, unavailable(err)
		}
		if !used {
			return Principal{}, rejected(403, "PC用の一時キーが無効です。")
		}
		return Principal{Kind: CLI}, nil
	}
	return Principal{}, rejected(401, "受付への接続が必要です。")
}

func (a *Authenticator) bearer(r *http.Request) (Principal, *contract.Problem) {
	if len(r.Header.Values("Authorization")) != 1 {
		return Principal{}, rejected(401, "トークンが無効か、失効しています。")
	}
	match := bearerPattern.FindStringSubmatch(r.Header.Get("Authorization"))
	if len(match) != 2 || !tokenPattern.MatchString(match[1]) {
		return Principal{}, rejected(401, "トークンが無効か、失効しています。")
	}
	kind := PC
	lookup := a.store.PCToken
	if strings.HasPrefix(match[1], "idh_") {
		kind, lookup = Hostd, a.store.HostdToken
	}
	token, found, err := lookup(r.Context(), Hash(match[1]))
	if err != nil {
		return Principal{}, unavailable(err)
	}
	if !found || token.Name == "" {
		return Principal{}, rejected(401, "トークンが無効か、失効しています。")
	}
	if kind == PC {
		if token.ID == "" {
			return Principal{}, unavailable(errors.New("PCトークンのIDがありません"))
		}
		if err := a.touchPCToken(r.Context(), token.ID); err != nil {
			return Principal{}, unavailable(err)
		}
	}
	return Principal{Kind: kind, Name: token.Name, TokenID: token.ID, Delegated: kind == PC && token.Delegated}, nil
}

// touchPCToken records the token use unless it was recorded within touchInterval.
// The lookup just before this already rejects deleted tokens, so skipping the write keeps revocation immediate.
func (a *Authenticator) touchPCToken(ctx context.Context, id string) error {
	now := a.config.Now()
	a.touchMu.Lock()
	last, seen := a.touched[id]
	a.touchMu.Unlock()
	if seen && now.Sub(last) >= 0 && now.Sub(last) < touchInterval {
		return nil
	}
	if err := a.store.TouchPCToken(ctx, id, now); err != nil {
		return err
	}
	a.touchMu.Lock()
	if a.touched == nil || len(a.touched) > 10000 {
		a.touched = map[string]time.Time{}
	}
	a.touched[id] = now
	a.touchMu.Unlock()
	return nil
}
