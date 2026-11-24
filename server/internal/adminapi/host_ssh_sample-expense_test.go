package adminapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 偽のdockerは、稼働Workerの一覧・イメージの確認・docker run の引数と展開先の記録だけを行う。
const fakeVerifyDocker = `#!/bin/sh
case "$1" in
ps) printf '%s' "$FAKE_PS" ;;
inspect) echo sha256:workerimage ;;
rm) echo "$*" >> "$FAKE_LOG/rm"; kill "$(cat "$FAKE_LOG/runpid" 2>/dev/null)" 2>/dev/null; exit 0 ;;
run) echo $$ > "$FAKE_LOG/runpid" ;;
*) exit 42 ;;
esac
[ "$1" = run ] || exit 0
all="$*"
while [ $# -gt 0 ]; do
  if [ "$1" = -v ]; then dir=${2%%:*}; echo "$dir" > "$FAKE_LOG/dir"; [ -f "$dir/marker.txt" ] && echo seen > "$FAKE_LOG/marker"; fi
  shift
done
echo "$all" > "$FAKE_LOG/args"
[ -z "$FAKE_SLEEP" ] || exec sleep "$FAKE_SLEEP"
i=0; while [ $i -lt 3000 ]; do echo "filler line number $i padding padding padding"; i=$((i+1)); done
echo "PASSWORD=hunter2"
echo "LAST LINE"
exit "$FAKE_CODE"
`

func verifyTarGz(t *testing.T, valid bool) []byte {
	t.Helper()
	if !valid {
		return []byte("tar.gzではない入力")
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "marker.txt", Mode: 0600, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// verifyShells は固定操作を流すシェル。接続先の /bin/sh は bash とは限らない（Ubuntuは dash）ので、dash があれば両方で流す。
func verifyShells() []string {
	shells := []string{"/bin/sh"}
	for _, shell := range []string{"/usr/bin/dash", "/bin/dash"} {
		if _, err := os.Stat(shell); err == nil {
			return append(shells, shell)
		}
	}
	return shells
}

// shell は固定操作の先頭の /bin/sh の代わりに使う。
func verifyCommand(t *testing.T, shell, operation, ps, code string, stdin []byte, env ...string) (*exec.Cmd, string) {
	t.Helper()
	p, _ := distributedReadOnlyPolicy(t)
	bin, logs := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeVerifyDocker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(logs, "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	command, ok := strings.CutPrefix(p.Operations[operation], "/bin/sh -c ")
	if !ok {
		t.Fatalf("固定操作が /bin/sh -c で始まりません: %s", operation)
	}
	cmd := exec.Command("/bin/sh", "-c", shell+" -c "+command)
	cmd.Env = append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_PS="+ps, "FAKE_LOG="+logs, "FAKE_CODE="+code, "TMPDIR="+filepath.Join(logs, "tmp")), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd, logs
}

// verifyTemps は固定操作が TMPDIR に残した作業場所と出力。試験ごとに TMPDIR を分けるので、ほかの実行の物は混ざらない。
func verifyTemps(t *testing.T, logs string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(logs, "tmp", "host-ssh-verify*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func runVerifyOperation(t *testing.T, shell, operation, ps, code string, stdin []byte) (string, string, error) {
	t.Helper()
	cmd, logs := verifyCommand(t, shell, operation, ps, code, stdin)
	out, err := cmd.CombinedOutput()
	return string(out), logs, err
}

// 接続が切れて出力先が閉じたら、テストの終了を待たずに（実行中でも）容器の削除と作業場所の片付けを済ませる。
func TestSampleExpenseVerifyCleansUpWhenOutputClosed(t *testing.T) {
	for _, shell := range verifyShells() {
		for _, operation := range []string{"verify-sample-expense-server", "verify-sample-expense-client"} {
			// before: 起動前から切れている。running: 実行中に `.` を2行読んでから切る。
			for _, when := range []string{"before", "running"} {
				t.Run(filepath.Base(shell)+"/"+operation+"/"+when, func(t *testing.T) {
					cmd, logs := verifyCommand(t, shell, operation, "container1\n", "0", verifyTarGz(t, true), "FAKE_SLEEP=60")
					r, w, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					if when == "before" {
						if err = r.Close(); err != nil {
							t.Fatal(err)
						}
					}
					cmd.Stdout, cmd.Stderr = w, w
					started := time.Now()
					if err = cmd.Start(); err != nil {
						t.Fatal(err)
					}
					if cerr := w.Close(); cerr != nil {
						t.Fatal(cerr)
					}
					if when == "running" {
						got := make([]byte, 4)
						if _, rerr := io.ReadFull(r, got); rerr != nil || string(got) != ".\n.\n" {
							t.Fatalf("実行中の見張りの出力がありません: %q %v", got, rerr)
						}
						if rerr := r.Close(); rerr != nil {
							t.Fatal(rerr)
						}
					}
					err = cmd.Wait()
					if err == nil {
						t.Fatal("出力できなかったのに成功で終わっています")
					}
					if elapsed := time.Since(started); elapsed > 30*time.Second {
						t.Fatalf("切断の後もテストの終了を待っています: %s", elapsed)
					}
					removed, err := os.ReadFile(filepath.Join(logs, "rm"))
					if err != nil || !strings.Contains(string(removed), "-f host-ssh-verify-") {
						t.Fatalf("容器を消していません: %q %v", removed, err)
					}
					if left := verifyTemps(t, logs); len(left) > 0 {
						t.Fatalf("作業場所・出力が残っています: %v", left)
					}
					if pid, err := os.ReadFile(filepath.Join(logs, "runpid")); err == nil {
						if exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() == nil {
							t.Fatalf("docker run が残っています: %s", pid)
						}
					}
				})
			}
		}
	}
}

func TestSampleExpenseVerifyOperations(t *testing.T) {
	p, _ := distributedReadOnlyPolicy(t)
	for _, name := range p.ReadOnlyOperations {
		if name == "verify" || strings.HasPrefix(name, "verify-") {
			t.Fatalf("自動承認の対象に入れてはいけません: %s", name)
		}
	}
	cases := []struct {
		operation string
		wantEnv   []string
		wantCmd   string
	}{
		{"verify-sample-expense-server", []string{"GRADLE_USER_HOME=/tmp/gradle", "HOME=/tmp", "CI=true"}, "gradle --no-daemon test"},
		{"verify-sample-expense-client", []string{"PUB_CACHE=/tmp/pub-cache", "HOME=/tmp", "FLUTTER_SUPPRESS_ANALYTICS=true", "CI=true"}, "flutter pub get && (cd tool/lint && dart pub get) && flutter test && (cd tool/lint && dart analyze && dart test) && dart run tool/mason/verify_templates.dart"},
	}
	for _, shell := range verifyShells() {
		for _, tc := range cases {
			for _, code := range []string{"0", "7"} {
				t.Run(filepath.Base(shell)+"/"+tc.operation+"/"+code, func(t *testing.T) {
					out, logs, err := runVerifyOperation(t, shell, tc.operation, "container1\n", code, verifyTarGz(t, true))
					if (err == nil) != (code == "0") {
						t.Fatalf("終了コードが伝わりません: %v\n%s", err, out[max(0, len(out)-300):])
					}
					if !strings.Contains(out, "verify="+code) || !strings.Contains(out, "LAST LINE") {
						t.Fatalf("結果の表示がありません: %s", out[max(0, len(out)-300):])
					}
					if strings.Contains(out, "hunter2") {
						t.Fatal("秘匿すべき行が出ています")
					}
					if len(out) > 65536+100 {
						t.Fatalf("出力が切り詰められていません: %d", len(out))
					}
					args, err := os.ReadFile(filepath.Join(logs, "args"))
					if err != nil {
						t.Fatal(err)
					}
					for _, want := range append(tc.wantEnv, "--pull never", "--init", "--cap-drop ALL", "--security-opt no-new-privileges:true", "--group-add 1001", "--user ", " sha256:workerimage -c "+tc.wantCmd) {
						if !strings.Contains(string(args), want) {
							t.Fatalf("docker run の指定に %q がありません: %s", want, args)
						}
					}
					if _, err = os.Stat(filepath.Join(logs, "marker")); err != nil {
						t.Fatal("展開した作業ツリーが /work に渡っていません")
					}
					dir, err := os.ReadFile(filepath.Join(logs, "dir"))
					if err != nil {
						t.Fatal(err)
					}
					if filepath.Dir(strings.TrimSpace(string(dir))) != filepath.Join(logs, "tmp") {
						t.Fatalf("作業場所が TMPDIR に作られていません: %s", dir)
					}
					if _, err = os.Stat(strings.TrimSpace(string(dir))); !os.IsNotExist(err) {
						t.Fatalf("展開先が消えていません: %s", dir)
					}
				})
			}
		}
	}
}

func TestSampleExpenseVerifyRefusesBeforeRunning(t *testing.T) {
	cases := []struct {
		name, ps string
		valid    bool
		want     string
	}{
		{"稼働Workerが0件", "", true, "一意に確認できません"},
		{"稼働Workerが2件", "a\nb\n", true, "一意に確認できません"},
		{"不正なtar.gz", "container1\n", false, "展開できません"},
	}
	for _, shell := range verifyShells() {
		for _, operation := range []string{"verify-sample-expense-server", "verify-sample-expense-client"} {
			for _, tc := range cases {
				t.Run(filepath.Base(shell)+"/"+operation+"/"+tc.name, func(t *testing.T) {
					out, logs, err := runVerifyOperation(t, shell, operation, tc.ps, "0", verifyTarGz(t, tc.valid))
					if err == nil || !strings.Contains(out, tc.want) {
						t.Fatalf("理由を表示して失敗していません: %v %s", err, out)
					}
					if _, e := os.Stat(filepath.Join(logs, "args")); e == nil {
						t.Fatal("失敗すべき入力で docker run が呼ばれています")
					}
				})
			}
		}
	}
}
