// Package admincli provides local administration without exporting token issuance over HTTP.
package admincli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/startup"
	"issue-desk-intake/server/internal/store"
)

var errUsage = errors.New("使い方: intake admin --config <設定ファイル> --application-dir <アプリの場所> hostd-token issue|revoke <PC名> / pc-token issue|list|revoke [PC名] / client-release register（JSONは標準入力）|previous [flavor] / cli-key issue|revoke（取り消す値は標準入力） / claude-api-key set|clear（キーは標準入力） / usage-per-issue report --from YYYY-MM-DD --to YYYY-MM-DD")

func Run(ctx context.Context, args []string, in io.Reader, out, notice io.Writer) error {
	flags := flag.NewFlagSet("intake admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("config", "config.local.json", "")
	root := flags.String("application-dir", ".", "")
	declared := flags.String("settings", "", "")
	if flags.Parse(args) != nil {
		return errUsage
	}
	args = flags.Args()
	if len(args) < 2 {
		return errUsage
	}
	group, action := args[0], args[1]
	name := ""
	if len(args) == 3 {
		name = args[2]
	}
	if err := checkCommandArgs(group, action, name, args); err != nil {
		return err
	}
	app, err := filepath.Abs(*root)
	if err != nil {
		return errors.New("アプリの場所を解決できません")
	}
	if *declared == "" {
		*declared = filepath.Join(app, "deploy", "nas", "intake-settings.json")
	}
	config, err := startup.LoadLocal(*file, app, *declared)
	if err != nil {
		return err
	}
	if group == "claude-api-key" {
		return claudeAPIKey(config, action, in)
	}
	if group == "cli-key" {
		return cliKey(ctx, config, action, in, out)
	}
	if group == "pc-token" {
		return pc(ctx, config, action, name, out, notice)
	}
	dsn := os.Getenv("ISSUE_DESK_DATABASE_URL")
	if dsn == "" && (os.Getenv("PGHOST") == "" || os.Getenv("PGUSER") == "" || os.Getenv("PGDATABASE") == "") {
		return errors.New("共有DBの接続環境変数を設定したNASの受付で実行してください")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		return errors.New("共有DBへ接続できません。接続環境変数と稼働状態を確認してください")
	}
	defer db.Close()
	if group == "hostd-token" {
		return hostdToken(ctx, db, action, name, out, notice)
	}
	if group == "usage-per-issue" {
		return usageReport(ctx, db, config, args[2:], out)
	}
	if action == "previous" {
		return previousRelease(ctx, db, name, out)
	}
	return registerRelease(ctx, db, config, in, notice)
}

// checkCommandArgs validates the command group, action and target before any configuration is read.
func checkCommandArgs(group, action, name string, args []string) error {
	switch group {
	case "usage-per-issue":
		if action != "report" {
			return errUsage
		}
		if _, _, _, _, _, err := parseUsageReportArgs(args[2:]); err != nil {
			return err
		}
	case "claude-api-key":
		if len(args) != 2 || action != "set" && action != "clear" {
			return errUsage
		}
	case "cli-key":
		if len(args) != 2 || action != "issue" && action != "revoke" {
			return errUsage
		}
	case "hostd-token":
		if len(args) != 3 || !fleet.ValidHost(name) || action != "issue" && action != "revoke" {
			return errUsage
		}
	case "pc-token":
		if action == "list" {
			if len(args) != 2 {
				return errUsage
			}
		} else if len(args) != 3 || action != "issue" && action != "revoke" {
			return errUsage
		} else if action == "issue" {
			if err := validatePCIssueName(name); err != nil {
				return err
			}
		} else if !fleet.ValidHost(name) {
			return errUsage
		}
	case "client-release":
		if action == "previous" {
			if len(args) > 3 || len(args) == 3 && name != "-python" && name != "" {
				return errUsage
			}
		} else if len(args) != 2 || action != "register" {
			return errUsage
		}
	default:
		return errUsage
	}
	return nil
}

// hostdToken issues or revokes the hostd token of a PC.
func hostdToken(ctx context.Context, db *sql.DB, action, name string, out, notice io.Writer) error {
	if action == "revoke" {
		if _, err := db.ExecContext(ctx, "DELETE FROM host_image_tokens WHERE host=$1", name); err != nil {
			return errors.New("hostdトークンを失効できません")
		}
		fmt.Fprintln(notice, "hostdトークンを失効させました。")
		return nil
	}
	token, err := newToken("idh_")
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, "INSERT INTO host_image_tokens(host,hash)VALUES($1,$2)ON CONFLICT DO NOTHING", name, hash(token))
	if err != nil {
		return errors.New("hostdトークンを発行できません。共有DBを確認してください")
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return errors.New("有効なhostdトークンがあるため発行しません。必要なら先に失効してください")
	}
	fmt.Fprintln(notice, "hostdトークンを発行しました。標準出力の値は今回だけ表示します。保護されたGit管理外のファイルへ保存してください。")
	_, err = fmt.Fprintln(out, token)
	return err
}

// previousRelease prints the registered desired revision of the flavor.
func previousRelease(ctx context.Context, db *sql.DB, name string, out io.Writer) error {
	var revision string
	if err := db.QueryRowContext(ctx, "SELECT revision FROM releases WHERE host='*' AND flavor=$1", name).Scan(&revision); err != nil {
		return errors.New("登録済みの望む版を読めません")
	}
	_, err := fmt.Fprintln(out, revision)
	return err
}

// registerRelease registers the desired revision read as JSON from standard input.
func registerRelease(ctx context.Context, db *sql.DB, config *startup.Config, in io.Reader, notice io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(in, (1<<20)+1))
	decoder.UseNumber()
	var body map[string]any
	if decoder.Decode(&body) != nil || body == nil {
		return errors.New("版のJSONを標準入力から読み取れません")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("版のJSONに余分な内容があります")
	}
	f := &fleet.Service{DB: db, FleetPath: config.FleetPath, AssignmentsPath: config.AssignmentsPath}
	service := &hostd.Service{DB: db, Targets: f.Targets, Assignments: func() (*fleet.Assignments, error) { _, a, e := f.Load(); return a, e }}
	registered, err := service.Register(ctx, body)
	if err != nil {
		return errors.New("望む版を登録できません。JSONの項目・担当表・共有DBを確認してください")
	}
	fmt.Fprintln(notice, "望む版を登録しました。")
	if result, ok := registered.(map[string]any); ok {
		if warnings, ok := result["warnings"].([]string); ok {
			for _, warning := range warnings {
				fmt.Fprintln(notice, "警告: "+warning)
			}
		}
	}
	return nil
}

func newToken(prefix string) (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", errors.New("トークンの乱数を生成できません")
	}
	return prefix + base64.RawURLEncoding.EncodeToString(bytes), nil
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func uuid() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("トークンIDを生成できません")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
