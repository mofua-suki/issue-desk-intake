package startup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/fleet"
)

type Config struct {
	File           string
	ApplicationDir string
	Values         map[string]any
	RuntimeDir     string
	// SQLiteFile is the SQLite-era database path. It is only checked to refuse a
	// start while an unmigrated file remains; nothing is stored there any more.
	SQLiteFile string
	// LedgerFile is the number ledger (JSONL). It alone decides where the ledger is.
	LedgerFile string
	// ApplicationSchema is empty in production (the default application schema,
	// with the switch-over flag required). Only tests set it to use a throwaway schema.
	ApplicationSchema string
	FleetPath         string
	AssignmentsPath   string
	Codex             string
	Claude            string
	TokenFile         string
	Port              int
	Bind              string
	PublicURL         string
	Repositories      []string
	RepositoryEntries []any
	// AppPostgresDSN is the application store connection (ISSUE_DESK_DATABASE_URL); PG* variables are the fallback.
	AppPostgresDSN string
	// groupWarnings suppresses repeated warnings about disabled repository
	// groups. Copies of a Config and reloaded Configs share it; nil uses the
	// process-wide state.
	groupWarnings *repositoryGroupWarnings
}

func readObject(file string) (map[string]any, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("設定ファイルを開けません")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("設定ファイルを上限内で読み取れません")
	}
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil {
		return nil, errors.New("設定ファイルのJSONが不正です")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("設定ファイルに余分な内容があります")
	}
	return value, nil
}
func absolute(dir, file string) string {
	if filepath.IsAbs(file) {
		return filepath.Clean(file)
	}
	return filepath.Clean(filepath.Join(dir, file))
}
func command(dir, value string, search bool) string {
	if search && !strings.ContainsAny(value, "/\\") {
		if resolved, err := exec.LookPath(value); err == nil {
			return resolved
		}
		return value
	}
	return absolute(dir, value)
}
func forbiddenSecretKey(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if regexp.MustCompile(`(?i)token|password|secret`).MatchString(key) || forbiddenSecretKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if forbiddenSecretKey(child) {
				return true
			}
		}
	}
	return false
}
func Load(file, applicationDir, declaredFile string) (*Config, error) {
	config, err := loadLocal(file, applicationDir, declaredFile)
	if err != nil {
		return nil, err
	}
	if err = config.mergeFleetRepositories(); err != nil {
		return nil, err
	}
	return config, nil
}

// LoadLocal reads the settings needed by management commands without starting the server.
func LoadLocal(file, applicationDir, declaredFile string) (*Config, error) {
	config, err := loadLocal(file, applicationDir, declaredFile)
	if err != nil {
		return nil, err
	}
	if len(config.Repositories) == 0 {
		return nil, errors.New("Serverのリポジトリ一覧を設定してください")
	}
	return config, nil
}

func loadLocal(file, applicationDir, declaredFile string) (*Config, error) {
	file, err := filepath.Abs(file)
	if err != nil {
		return nil, errors.New("設定ファイルのパスを解決できません")
	}
	applicationDir, err = filepath.Abs(applicationDir)
	if err != nil {
		return nil, errors.New("アプリのパスを解決できません")
	}
	value, err := readObject(file)
	if err != nil {
		return nil, err
	}
	if declaredFile != "" {
		declared, readErr := readObject(declaredFile)
		if readErr != nil {
			if _, statErr := os.Stat(declaredFile); !errors.Is(statErr, os.ErrNotExist) {
				return nil, readErr
			}
		} else {
			if forbiddenSecretKey(declared) {
				return nil, errors.New("宣言設定に資格情報の項目は置けません")
			}
			for key, entry := range declared {
				if key != "projectCreation" {
					return nil, errors.New("宣言設定に未対応の項目があります")
				}
				value[key] = entry
			}
		}
	}
	return normalize(file, applicationDir, value)
}

func normalize(file, applicationDir string, value map[string]any) (*Config, error) {
	dir := filepath.Dir(file)
	var err error
	if value["serverUrl"] != nil || value["serverTokenFile"] != nil {
		return nil, errors.New("ServerにはServer用の設定ファイルを指定してください")
	}
	intake := fleet.Object(value["intake"])
	if intake == nil {
		intake = map[string]any{}
		value["intake"] = intake
	}
	if intake["remoteUrl"] != nil || intake["remoteTokenFile"] != nil || intake["workerStateDir"] != nil {
		return nil, errors.New("Serverで使用しない設定が残っています")
	}
	if value["workerId"] != "nas" {
		return nil, errors.New("Serverの配置担当はnasを指定してください")
	}
	if _, err = serverURL(fleet.Text(value["giteaUrl"])); err != nil {
		return nil, err
	}
	value["giteaUrl"] = strings.TrimRight(fleet.Text(value["giteaUrl"]), "/")
	if public := fleet.Text(value["publicUrl"]); public != "" {
		if _, err = serverURL(public); err != nil {
			return nil, err
		}
		value["publicUrl"] = strings.TrimRight(public, "/")
	}
	if err = normalizeFiles(dir, applicationDir, value); err != nil {
		return nil, err
	}
	if err = normalizeGiteaAccounts(value); err != nil {
		return nil, err
	}
	normalizeCommands(dir, value)
	port, bind, public, err := normalizeListen(intake)
	if err != nil {
		return nil, err
	}
	repositories, err := configRepositories(value)
	if err != nil {
		return nil, err
	}
	if err := checkAppStoreEnv(); err != nil {
		return nil, err
	}
	runtime := fleet.Text(value["runtimeDir"])
	return &Config{AppPostgresDSN: os.Getenv("ISSUE_DESK_DATABASE_URL"), File: file, ApplicationDir: applicationDir, Values: value, RuntimeDir: runtime, SQLiteFile: filepath.Join(runtime, "state", "intake.sqlite"), LedgerFile: filepath.Join(runtime, "state", "intake-desk-numbers.jsonl"), FleetPath: filepath.Join(applicationDir, "fleet.json"), AssignmentsPath: filepath.Join(applicationDir, "assignments.json"), Codex: fleet.Text(value["codex"]), Claude: fleet.Text(value["claude"]), TokenFile: fleet.Text(value["intakeTokenFile"]), Port: port, Bind: bind, PublicURL: public, Repositories: repositories, groupWarnings: &repositoryGroupWarnings{}}, nil
}

// normalizeFiles は保存先・認証ファイル・方針ファイルなどのパスを設定ファイルの場所から絶対パスにし、ホスト作業用SSHの方針の既定を補う。
func normalizeFiles(dir, applicationDir string, value map[string]any) error {
	for _, key := range []string{"runtimeDir", "tokenFile", "codex"} {
		text := fleet.Text(value[key])
		if strings.TrimSpace(text) == "" {
			return errors.New("Serverの保存先・認証ファイル・Codex実行ファイルを設定してください")
		}
		value[key] = absolute(dir, text)
	}
	for _, key := range []string{"workerTokenFile", "intakeTokenFile", "sshRepairPolicyFile", "hostSshPolicyFile", "permissionGrantRulesFile", "decisionMirrorFile"} {
		if text := fleet.Text(value[key]); text != "" {
			value[key] = absolute(dir, text)
		}
	}
	// ホスト作業用SSHの方針は、指定が無ければ配布物の host-ssh.json を使う（指定が無いと、申請も許可の登録も常に503になる）。
	if fleet.Text(value["hostSshPolicyFile"]) == "" && fleet.Text(value["sshRepairPolicyFile"]) == "" {
		value["hostSshPolicyFile"] = filepath.Join(applicationDir, "host-ssh.json")
	}
	return nil
}

// normalizeGiteaAccounts はWorkerとServerのGitea認証ファイルを同じ保護フォルダーの別ファイルに揃え、Serverのアカウント名を検査する。
func normalizeGiteaAccounts(value map[string]any) error {
	token := fleet.Text(value["tokenFile"])
	private := filepath.Dir(token)
	if value["intakeTokenFile"] == nil {
		value["intakeTokenFile"] = filepath.Join(private, "gitea-intake-token")
	}
	if value["workerTokenFile"] == nil {
		value["workerTokenFile"] = filepath.Join(private, "worker-token")
	}
	for _, key := range []string{"workerTokenFile", "intakeTokenFile"} {
		path := fleet.Text(value[key])
		if filepath.Dir(path) != private || path == token {
			return errors.New("Giteaの認証ファイルは同じ保護フォルダーの別ファイルにしてください")
		}
	}
	if value["workerTokenFile"] == value["intakeTokenFile"] {
		return errors.New("WorkerとServerの認証ファイルを区別してください")
	}
	if value["intakeAccount"] == nil {
		value["intakeAccount"] = "issue-desk-intake"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(fleet.Text(value["intakeAccount"])) {
		return errors.New("ServerのGiteaアカウント名が不正です")
	}
	return nil
}

// normalizeCommands は Claude・git・node・pnpm・python の実行ファイルの指定を解決する。Claude は未指定なら ISSUE_DESK_CLAUDE を使う。
func normalizeCommands(dir string, value map[string]any) {
	if value["claude"] == nil || value["claude"] == "" {
		if executable := os.Getenv("ISSUE_DESK_CLAUDE"); executable != "" {
			value["claude"] = executable
		}
	}
	if text := fleet.Text(value["claude"]); text != "" {
		value["claude"] = command(dir, text, true)
	}
	for _, key := range []string{"git", "node", "pnpm", "python"} {
		if text := fleet.Text(value[key]); text != "" {
			value[key] = command(dir, text, key == "pnpm" || key == "python")
		}
	}
}

// normalizeListen は受付の待受ポート・アドレス・公開URLを検査して返し、AIの同時実行数の既定を補う。
func normalizeListen(intake map[string]any) (int, string, string, error) {
	port := 8787
	if raw, ok := intake["port"]; ok {
		n := fleet.Number(raw)
		if n < 1 || n > 65535 || n != float64(int(n)) {
			return 0, "", "", errors.New("Serverのポート番号が不正です")
		}
		port = int(n)
	}
	bind := fleet.Text(intake["bind"])
	if bind == "" {
		bind = "127.0.0.1"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`).MatchString(bind) {
		return 0, "", "", errors.New("Serverの待受アドレスが不正です")
	}
	public := strings.TrimRight(fleet.Text(intake["publicUrl"]), "/")
	if public != "" {
		u, err := serverURL(public)
		if err != nil || u.Path != "" && u.Path != "/" {
			return 0, "", "", errors.New("Serverの公開URLが不正です")
		}
	}
	if raw, ok := intake["aiConcurrency"]; ok {
		n := fleet.Number(raw)
		if n < 1 || n != float64(int(n)) {
			return 0, "", "", errors.New("AIの同時実行数は1以上の整数で指定してください")
		}
	} else {
		intake["aiConcurrency"] = 3
	}
	return port, bind, public, nil
}

// configRepositories は受付が扱うリポジトリの名前を、形と大文字小文字を無視した重複を検査して並べる。
func configRepositories(value map[string]any) ([]string, error) {
	repositories := []string{}
	seen := map[string]bool{}
	for _, raw := range fleet.List(value["repositories"]) {
		repo := fleet.Object(raw)
		name := fleet.Text(repo["name"])
		if !fleet.ValidRepo(name) || seen[strings.ToLower(name)] {
			return nil, errors.New("Serverのリポジトリ一覧が不正です")
		}
		seen[strings.ToLower(name)] = true
		repositories = append(repositories, name)
	}
	return repositories, nil
}

// NumberLedgerPath returns the number ledger path. An empty LedgerFile is
// refused by numbers.OpenLedger instead of being derived from another setting.
func (c *Config) NumberLedgerPath() string { return c.LedgerFile }

// mergeFleetRepositories keeps the local settings intact for project registration.
func (c *Config) mergeFleetRepositories() error {
	doc, _, err := (&fleet.Service{FleetPath: c.FleetPath, AssignmentsPath: c.AssignmentsPath}).Load()
	if err != nil {
		if errors.Is(err, fleet.ErrPlatform) {
			return err
		}
		return errors.New("fleetと担当表を読み取れません")
	}
	entries := append([]any{}, fleet.List(fleet.Clone(c.Values["repositories"]))...)
	seen := map[string]bool{}
	byName := map[string]map[string]any{}
	for _, name := range c.Repositories {
		seen[strings.ToLower(name)] = true
	}
	for _, item := range entries {
		entry := fleet.Object(item)
		byName[strings.ToLower(fleet.Text(entry["name"]))] = entry
	}
	hosts := make([]string, 0, len(doc.Hosts))
	for host := range doc.Hosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		config := doc.Hosts[host]
		raw, exists := config["repositories"]
		if !exists {
			raw = doc.Defaults["repositories"]
		}
		if (exists || raw != nil) && fleet.List(raw) == nil {
			return errors.New("fleetのリポジトリ一覧が不正です")
		}
		for _, item := range fleet.List(raw) {
			repo := fleet.Object(item)
			name := fleet.Text(repo["name"])
			if !fleet.ValidRepo(name) {
				return errors.New("fleetのリポジトリ一覧が不正です")
			}
			key := strings.ToLower(name)
			if !seen[key] {
				seen[key] = true
				c.Repositories = append(c.Repositories, name)
				entry := map[string]any{"name": name}
				entries = append(entries, entry)
				byName[key] = entry
			}
			for _, field := range []string{"displayName", "description"} {
				if fleet.Text(byName[key][field]) == "" && fleet.Text(repo[field]) != "" {
					byName[key][field] = repo[field]
				}
			}
		}
	}
	if len(c.Repositories) == 0 {
		return errors.New("Serverのリポジトリ一覧を設定してください")
	}
	c.RepositoryEntries = entries
	return nil
}
func serverURL(input string) (*url.URL, error) {
	u, err := url.Parse(input)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("ServerのURL設定が不正です")
	}
	return u, nil
}
func (c *Config) Address() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }

// checkAppStoreEnv rejects the retired ISSUE_DESK_APP_STORE=sqlite. The store is
// always PostgreSQL now, so "postgres" (or unset) is accepted and anything else
// fails loudly instead of being ignored.
func checkAppStoreEnv() error {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ISSUE_DESK_APP_STORE"))) {
	case "", "postgres":
		return nil
	}
	return errors.New("ISSUE_DESK_APP_STOREにsqliteは使えません。保存先はPostgreSQLだけです（postgresまたは未設定にしてください）")
}
