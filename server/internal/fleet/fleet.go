package fleet

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"regexp"
	"sync"
	"time"
)

type Document struct {
	Version  int                       `json:"version"`
	Defaults map[string]any            `json:"defaults"`
	Hosts    map[string]map[string]any `json:"hosts"`
}

type Service struct {
	loadMu          sync.Mutex
	loadCache       *loadCache
	WebhookVerifier func(context.Context, string, map[string]any) (string, error)
	DB              *sql.DB
	FleetPath       string
	AssignmentsPath string
	Now             func() time.Time
	ReleasePolicy   func(string, map[string]any) map[string]any
}

//go:embed defaults.json
var defaultsJSON []byte

var hostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
var ErrPlatform = errors.New("fleetのenvironmentのplatformはlinux・darwin・nullのいずれかにしてください")
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var secretKeyPattern = regexp.MustCompile(`(?i)token|password|secret|authorization|credential`)
var secretValuePattern = regexp.MustCompile(`\b(?:idk_|idh_)[A-Za-z0-9_-]{20,}|https?://[^/\s]+@`)

func ValidHost(host string) bool { return hostPattern.MatchString(host) }
func ValidRepo(repo string) bool { return repoPattern.MatchString(repo) }
