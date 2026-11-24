package hostd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

func (s *Service) Register(ctx context.Context, body map[string]any) (any, error) {
	if !only(body, "host", "flavor", "registry", "tag", "digest", "revision", "baseRevision", "changedFiles") {
		return nil, problem(400, "版に使えない項目があります。")
	}
	host, flavor, revision, tag, err := validateRelease(body)
	if err != nil {
		return nil, err
	}
	base := body["baseRevision"]
	changes := body["changedFiles"]
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "LOCK TABLE releases IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return nil, err
	}
	var oldJSON []byte
	err = tx.QueryRowContext(ctx, "SELECT row_to_json(r)FROM releases r WHERE host=$1 AND flavor=$2", host, flavor).Scan(&oldJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var old map[string]any
	_ = json.Unmarshal(oldJSON, &old)
	if host == "*" && old["revision"] != revision {
		if err = s.startReleaseRollout(ctx, tx, flavor, revision, old); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(20261012,1)"); err != nil {
		return nil, err
	}
	kind, err := recordedReleaseKind(ctx, tx, host, flavor, revision, old, base, changes)
	if err != nil {
		return nil, err
	}
	warnings, err := releaseWarnings(ctx, tx, host, flavor, revision, old, body["registry"])
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO releases(host,flavor,registry,tag,digest,revision,kind)VALUES($1,$2,$3,$4,$5,$6,$7)ON CONFLICT(host,flavor)DO UPDATE SET registry=excluded.registry,tag=excluded.tag,digest=excluded.digest,revision=excluded.revision,kind=excluded.kind,created_at=CASE WHEN releases.revision=excluded.revision THEN releases.created_at ELSE now()END", host, flavor, body["registry"], tag, body["digest"], revision, kind); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"host": host, "flavor": flavor, "registry": body["registry"], "tag": tag, "digest": body["digest"], "revision": revision, "baseRevision": base, "changedFiles": changes, "kind": kind, "warnings": warnings}, nil
}

// validateRelease は登録する版の項目の形を検査し、対象のPC・種類・revision・tag を返す。
func validateRelease(body map[string]any) (string, string, string, string, error) {
	host := "*"
	if v, exists := body["host"]; exists {
		host = fleet.Text(v)
	}
	flavor := ""
	if v, exists := body["flavor"]; exists {
		flavor = fleet.Text(v)
		if v != flavor {
			return "", "", "", "", problem(400, "flavorの形が違います。")
		}
	}
	revision := fleet.Text(body["revision"])
	tag := fleet.Text(body["tag"])
	if host != "*" && !fleet.ValidHost(host) || !one(flavor, "", "-python") || !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?$`).MatchString(fleet.Text(body["registry"])) || !tagPattern.MatchString(tag) || !shaPattern.MatchString(revision) || !strings.HasPrefix(tag, revision[:12]) || !digestPattern.MatchString(fleet.Text(body["digest"])) {
		return "", "", "", "", problem(400, "版のregistry・tag・digest・revisionが不正です。")
	}
	base := body["baseRevision"]
	if base != nil && !shaPattern.MatchString(fleet.Text(base)) {
		return "", "", "", "", problem(400, "baseRevisionの形が違います。")
	}
	changes := body["changedFiles"]
	if changes != nil {
		files, ok := changes.([]any)
		if !ok {
			return "", "", "", "", problem(400, "changedFilesは文字列の配列です。")
		}
		for _, raw := range files {
			value, ok := raw.(string)
			if !ok || value == "" || strings.ContainsRune(value, '\n') {
				return "", "", "", "", problem(400, "changedFilesの形が違います。")
			}
		}
	}
	return host, flavor, revision, tag, nil
}

// startReleaseRollout は全PC向けの版が変わったとき、先行のPCで試す段取りを始め、前の段取りを置き換え済みにする。戻す版なら先行を挟まない。
func (s *Service) startReleaseRollout(ctx context.Context, tx *sql.Tx, flavor, revision string, old map[string]any) error {
	previousRelease := old
	var activeID int64
	var activeRevision, activeStatus string
	var previousRaw []byte
	err := tx.QueryRowContext(ctx, "SELECT id,revision,status,previous FROM release_rollouts WHERE host='*' AND flavor=$1 ORDER BY id DESC LIMIT 1 FOR UPDATE", flavor).Scan(&activeID, &activeRevision, &activeStatus, &previousRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var previous map[string]any
	_ = json.Unmarshal(previousRaw, &previous)
	rollback := activeID != 0 && activeStatus != "superseded" && previous["revision"] == revision
	if !rollback {
		if activeRevision == old["revision"] && one(activeStatus, "canary", "failed") {
			previousRelease = previous
		}
		canary, e := s.selectCanary(ctx, tx, flavor)
		if e != nil {
			return e
		}
		if previousRelease != nil && previousRelease["revision"] != revision && canary != "" {
			if _, err = tx.ExecContext(ctx, "INSERT INTO release_rollouts(host,flavor,revision,canary,status,previous,started_at)VALUES('*',$1,$2,$3,'canary',$4::jsonb,$5)", flavor, revision, canary, encode(previousRelease), s.now()); err != nil {
				return err
			}
		}
	}
	if activeID != 0 {
		if _, err = tx.ExecContext(ctx, "UPDATE release_rollouts SET status='superseded',decided_at=$2 WHERE id=$1", activeID, s.now()); err != nil {
			return err
		}
	}
	return nil
}

// recordedReleaseKind は版の反映の急ぎ具合を、記録済みならその値、無ければ前の版からの変更ファイルで決めて記録する。
func recordedReleaseKind(ctx context.Context, tx *sql.Tx, host, flavor, revision string, old map[string]any, base, changes any) (string, error) {
	kind := ""
	err := tx.QueryRowContext(ctx, "SELECT kind FROM client_release_history WHERE host=$1 AND flavor=$2 AND revision=$3", host, flavor, revision).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		kind = "align"
		if old["revision"] != nil && old["revision"] == base && changes != nil {
			kind = classifyRelease(fleet.Strings(changes))
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO client_release_history(host,flavor,revision,base_revision,kind,changed_files)VALUES($1,$2,$3,$4,$5,$6::jsonb)", host, flavor, revision, old["revision"], kind, encode(changes)); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return kind, nil
}

// releaseWarnings は登録する版で、PC別の版が全PC向けの版を上書きし続ける・取得元が変わるなどの注意を集める。
func releaseWarnings(ctx context.Context, tx *sql.Tx, host, flavor, revision string, old map[string]any, registry any) ([]string, error) {
	var err error
	warnings := []string{}
	if host == "*" {
		rows, e := tx.QueryContext(ctx, "SELECT host FROM releases WHERE host<>'*' AND flavor=$1 AND revision<>$2", flavor, revision)
		if e != nil {
			return nil, e
		}
		overridden := []string{}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			overridden = append(overridden, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if w := overrideWarning(flavor, overridden); w != "" {
			warnings = append(warnings, w)
		}
	} else if w := registryChangeWarning(host, fleet.Text(old["registry"]), fleet.Text(registry)); w != "" {
		warnings = append(warnings, w)
	}
	return warnings, nil
}

func classifyRelease(files []string) string {
	kind := "lazy"
	for _, file := range files {
		if strings.HasPrefix(file, "docs/") || file == "README.md" || strings.HasSuffix(file, "/README.md") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		if file == "deploy/versions.json" || file == "deploy/antigravity-release.json" ||
			strings.HasPrefix(file, "internal/intake/") || file == "internal/execution/config.go" {
			return "align"
		}
		if strings.HasPrefix(file, "internal/") || strings.HasPrefix(file, "cmd/") ||
			strings.HasPrefix(file, "deploy/client/") || file == "go.mod" || file == "go.sum" || file == "Dockerfile" {
			kind = "soon"
		}
	}
	return kind
}
func (s *Service) selectCanary(ctx context.Context, tx *sql.Tx, flavor string) (string, error) {
	a, err := s.Assignments()
	if err != nil {
		return "", err
	}
	successful := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT host FROM host_image_results WHERE flavor=$1 AND status='success'", flavor)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var host string
		if err = rows.Scan(&host); err != nil {
			rows.Close()
			return "", err
		}
		successful[host] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	for _, host := range a.ReleaseCanaryHosts {
		if !successful[host] {
			continue
		}
		var at time.Time
		var health []byte
		err = tx.QueryRowContext(ctx, "SELECT heartbeat_at,hostd_health FROM hosts WHERE host=$1", host).Scan(&at, &health)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		var value map[string]any
		_ = json.Unmarshal(health, &value)
		if !at.After(s.now()) && s.now().Sub(at) <= 20*time.Minute && value["cycle"] != nil {
			return host, nil
		}
	}
	return "", nil
}

// Tick evaluates saved rollout rows, independently of requests for a desired image.
func (s *Service) Tick(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT id FROM release_rollouts WHERE status='canary' ORDER BY id")
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if e := s.tickRollout(ctx, id); e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
