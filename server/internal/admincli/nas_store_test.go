package admincli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"text/template"
)

func TestNASInspectTemplateFiltersSecretsWithoutSplittingEnvironmentValues(t *testing.T) {
	// 偽Dockerの応答だけに依存せず、配備するinspect用Goテンプレート自体を実行する。
	raw, err := os.ReadFile("../../../deploy/nas/update-desk-access.sh")
	if err != nil {
		t.Fatal(err)
	}
	const start = "bounded docker inspect --format '"
	var format string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, start) && strings.Contains(line, ".Config.Env") {
			format = strings.SplitN(strings.TrimPrefix(line, start), "'", 2)[0]
		}
	}
	if format == "" {
		t.Fatal("配備用の環境取得テンプレートが見つかりません")
	}
	compiled, err := template.New("inspect").Funcs(template.FuncMap{"split": strings.Split}).Parse(format)
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"secret = with spaces", "secret\nPGHOST=other", "secret\r"} {
		entries := []string{"ISSUE_DESK_APP_STORE=postgres", "ISSUE_DESK_DATABASE_URL=host=db user=user dbname=db", "PGHOST=db", "PGUSER=user", "PGDATABASE=db", "PGPASSWORD=" + password, "OTHER_SECRET=must-not-appear"}
		var output bytes.Buffer
		if err := compiled.Execute(&output, map[string]any{"Config": map[string]any{"Env": entries}}); err != nil {
			t.Fatal(err)
		}
		want := strings.Join(entries[:6], "\n") + "\n"
		if strings.ContainsAny(password, "\r\n") {
			want = strings.Join(entries[:5], "\n") + "\nINVALID_STORE_ENV\n"
		}
		if output.String() != want {
			t.Fatal("配備するテンプレートの環境の抽出が不正です")
		}
	}
}

func TestNASStoreSnapshotRejectsNonPostgresBeforeOpeningDatabase(t *testing.T) {
	for _, expected := range []string{"sqlite", "secret-invalid"} {
		t.Setenv("ISSUE_DESK_EXPECTED_APP_STORE", expected)
		t.Setenv("ISSUE_DESK_APP_STORE", "postgres")
		t.Setenv("ISSUE_DESK_DATABASE_URL", "postgres://user:secret-password@127.0.0.1:1/private-db")
		args, file := fixture(t)
		for _, action := range []string{"issue", "revoke"} {
			out, notice, err := command(t, append(args, "cli-key", action), strings.Repeat("a", 43))
			if err == nil || out != "" || notice != "" {
				t.Fatal("PostgreSQLでない確認値なのに一時キーを操作しました")
			}
			if message := err.Error(); !strings.Contains(message, "保存先") || strings.Contains(message, "secret-") {
				t.Fatal("原因の分かる秘密の無い診断が必要です")
			}
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatal("不一致なのにSQLiteを開きました")
		}
	}
}

func TestNASPostgresConnectionErrorsNeverExposeSecrets(t *testing.T) {
	t.Setenv("ISSUE_DESK_EXPECTED_APP_STORE", "postgres")
	t.Setenv("ISSUE_DESK_APP_STORE", "postgres")
	t.Setenv("PGHOST", "")
	for _, dsn := range []string{"", " ", "postgres://user:secret-password@127.0.0.1:1/private-db?connect_timeout=1"} {
		t.Setenv("ISSUE_DESK_DATABASE_URL", dsn)
		args, file := fixture(t)
		out, notice, err := command(t, append(args, "cli-key", "issue"), "")
		if err == nil || out != "" || notice != "" || strings.Contains(err.Error(), "secret-password") || !strings.Contains(err.Error(), "PostgreSQL") {
			t.Fatal("接続設定の診断が不正です")
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatal("接続失敗でSQLiteへ戻りました")
		}
	}
}
