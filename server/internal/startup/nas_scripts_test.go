package startup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nasDockerStub = `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$NAS_TEST_LOG"
last='';for value in "$@";do last=$value;done
health(){
 container=$1
 if [ "$container" = entry ];then container=$(cat "$NAS_TEST_STATE/entry");fi
 role=standby;revision=$NAS_TEST_TARGET
 if [ "$container" = blue ];then
  revision=$NAS_TEST_OLD
  if [ "$(cat "$NAS_TEST_STATE/blue")" = true ] && [ "$(cat "$NAS_TEST_STATE/owner")" = blue ];then role=active;fi
 else
  if [ "${NAS_TEST_CASE:-}" = both ]||{ [ "$(cat "$NAS_TEST_STATE/owner")" = green ]&&[ "${NAS_TEST_CASE:-}" != rollback ];};then role=active;fi
 fi
 if [ "${NAS_TEST_CASE:-}" = none ];then role=standby;fi
 if [ "${NAS_TEST_CASE:-}" = same ];then revision=$NAS_TEST_TARGET;fi
 printf '{"db":{"ok":true},"ok":true,"ready":false,"revision":"%s","role":"%s","ui":"%s"}\n' "$revision" "$role" "$NAS_TEST_UI"
}
case "$1" in
compose)
 case " $* " in
  *' ps '*) case "$last" in *-entry)echo entry;;*-blue)echo blue;;*-green)echo green;;esac;;
  *' up '*) case "$last" in *-green)echo true >"$NAS_TEST_STATE/green";;esac;;
 esac;;
inspect) if [ "$last" = entry ];then echo true;else cat "$NAS_TEST_STATE/$last";fi;;
exec)
 container=$2
 case "$3" in
  node)
   script=$5
   if [ "${NAS_TEST_CASE:-}" = healthfail ]&&[ "$container" = green ];then echo '{"db":{"ok":true},"ok":false}';exit 1;fi
   output=$(health "$container")
   case "$script" in *'b.role==="active"'*) case "$output" in *'"role":"active"'*)exit 0;;*)exit 1;;esac;;esac
   printf '%s\n' "$output";;
  wget)case " $* " in *' -S '*)echo '  HTTP/1.1 200 OK' >&2;;*)health entry;;esac;;
  test)
   if [ "${NAS_TEST_CASE:-}" = native ]&&[ "$last" = /app/server/src/intake-handoff.mjs ];then exit 1;fi
   exit 0;;
  sh) echo "$7" >"$NAS_TEST_STATE/entry";;
  caddy)exit 0;;
 esac;;
run)
 case " $* " in
  *' --entrypoint node '*)while [ "$1" != -e ];do shift;done;shift;script=$1;shift;exec "$NAS_TEST_NODE" -e "$script" "$@";;
  *' /entry/active.caddy '*)echo 'reverse_proxy issue-desk-intake-blue:8787';;
  *'intake-heartbeat.json'*)
   revision=$NAS_TEST_OLD;[ "$(cat "$NAS_TEST_STATE/owner")" = blue ]||revision=$NAS_TEST_TARGET
   busy=false;[ "${NAS_TEST_CASE:-}" != busy ]||busy=true
   printf '{"time":9999999999999,"pid":1,"revision":"%s","busy":%s}\n' "$revision" "$busy";;
  *'intake-drain.status'*)echo '{"until":1,"phase":"pending"}';;
 esac;;
kill)echo green >"$NAS_TEST_STATE/owner";;
stop)echo false >"$NAS_TEST_STATE/$last";;
start)echo true >"$NAS_TEST_STATE/$last";[ "$last" != blue ]||echo blue >"$NAS_TEST_STATE/owner";;
*)exit 0;;
esac
`

func nasScripts(t *testing.T, mode string) (string, []string) {
	t.Helper()
	node, e := exec.LookPath("node")
	if e != nil {
		t.Skip("Docker内のNode処理を再現するNodeがありません")
	}
	root := t.TempDir()
	nas := filepath.Join(root, "deploy", "nas")
	bin := filepath.Join(root, "bin")
	state := filepath.Join(root, "state")
	for _, dir := range []string{nas, bin, state, filepath.Join(root, "home")} {
		if e = os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
	}
	entries, e := os.ReadDir(filepath.Join("..", "..", "..", "deploy", "nas"))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sh") {
			continue
		}
		raw, e := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(nas, entry.Name()), raw, 0700); e != nil {
			t.Fatal(e)
		}
	}
	entryDir := filepath.Join(nas, "entry")
	if e = os.MkdirAll(entryDir, 0700); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", "entry", "Caddyfile"))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(entryDir, "Caddyfile"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	for file, body := range map[string]string{filepath.Join(bin, "docker"): nasDockerStub, filepath.Join(state, "blue"): "true\n", filepath.Join(state, "green"): "false\n", filepath.Join(state, "entry"): "blue\n", filepath.Join(state, "owner"): "blue\n"} {
		if e = os.WriteFile(file, []byte(body), 0700); e != nil {
			t.Fatal(e)
		}
	}
	env := append(os.Environ(), "HOME="+filepath.Join(root, "home"), "PATH="+bin+":"+os.Getenv("PATH"), "NAS_TEST_LOG="+filepath.Join(root, "docker.log"), "NAS_TEST_STATE="+state, "NAS_TEST_NODE="+node, "NAS_TEST_CASE="+mode, "NAS_TEST_TARGET="+strings.Repeat("b", 40), "NAS_TEST_OLD="+strings.Repeat("a", 40), "NAS_TEST_UI="+strings.Repeat("c", 64), "ISSUE_DESK_UPDATE_TIMEOUT=0", "ISSUE_DESK_UPDATE_INTERVAL=1")
	return root, env
}
func runNASScript(t *testing.T, root string, env []string, name string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", append([]string{filepath.Join(root, "deploy", "nas", name)}, args...)...)
	command.Env = env
	output, e := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal(name, "試験が停止しない", string(output))
	}
	if e == nil {
		return 0, string(output)
	}
	if exit, ok := e.(*exec.ExitError); ok {
		return exit.ExitCode(), string(output)
	}
	t.Fatal(e)
	return -1, string(output)
}
func TestNASIntakeSelectsExactlyOneActiveServer(t *testing.T) {
	for _, mode := range []string{"same", "both", "none"} {
		t.Run(mode, func(t *testing.T) {
			root, env := nasScripts(t, mode)
			code, body := runNASScript(t, root, env, "intake.sh", "active")
			if mode == "same" {
				if code != 0 || strings.TrimSpace(body) != "blue" {
					t.Fatal(code, body)
				}
			} else if code == 0 {
				t.Fatal("複数または不在の書き手を選択", mode, body)
			}
		})
	}
}
func TestNASFailedStandbyAndBusyDrainKeepOldServer(t *testing.T) {
	for _, mode := range []string{"healthfail", "busy"} {
		t.Run(mode, func(t *testing.T) {
			root, env := nasScripts(t, mode)
			code, body := runNASScript(t, root, env, "deploy-server.sh", "--revision", strings.Repeat("b", 40))
			want := 1
			if mode == "busy" {
				want = 3
			}
			if code != want {
				t.Fatal(code, body)
			}
			old, e := os.ReadFile(filepath.Join(root, "state", "blue"))
			if e != nil || strings.TrimSpace(string(old)) != "true" {
				t.Fatal("旧色を停止", string(old), e)
			}
			log, _ := os.ReadFile(filepath.Join(root, "docker.log"))
			if strings.Contains(string(log), "stop blue") || strings.Contains(string(log), "kill --signal=SIGUSR2 blue") {
				t.Fatal("引継ぎ前に旧色を停止", string(log))
			}
			if green, _ := os.ReadFile(filepath.Join(root, "state", "green")); strings.TrimSpace(string(green)) != "false" {
				t.Fatal("失敗した新色を保持", string(green))
			}
		})
	}
}
func TestNASFailedTakeoverRollsBackToConfirmedOldServer(t *testing.T) {
	root, env := nasScripts(t, "rollback")
	env = append(env, "ISSUE_DESK_UPDATE_TIMEOUT=3")
	code, body := runNASScript(t, root, env, "deploy-server.sh", "--revision", strings.Repeat("b", 40))
	if code != 1 || !strings.Contains(body, "[server:switched-back:takeover]") {
		log, _ := os.ReadFile(filepath.Join(root, "docker.log"))
		t.Fatal(code, body, string(log))
	}
	for file, want := range map[string]string{"blue": "true", "green": "false", "entry": "blue", "owner": "blue"} {
		raw, e := os.ReadFile(filepath.Join(root, "state", file))
		if e != nil || strings.TrimSpace(string(raw)) != want {
			t.Fatal(file, string(raw), e)
		}
	}
}
func TestNASSameSHAUpdateDoesNotBuildOrRedeploy(t *testing.T) {
	root, env := nasScripts(t, "same")
	bin := filepath.Join(root, "bin")
	git := `#!/bin/sh
case " $* " in
 *' symbolic-ref '*)echo trunk;;
 *' rev-parse '*)echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb;;
esac
`
	if e := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "deploy", "nas", ".env"), []byte("ISSUE_DESK_TOOLS_IMAGE=tools:fixture\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "deploy", "nas", "update-skills.sh"), []byte("skills_listed(){ :; };skills_guard(){ :; }\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(root, "deploy", "nas", "update-desk-access.sh")); e != nil {
		t.Fatal(e)
	}
	code, body := runNASScript(t, root, env, "update.sh")
	if code != 0 {
		t.Fatal(code, body)
	}
	log, _ := os.ReadFile(filepath.Join(root, "docker.log"))
	if strings.Contains(string(log), "build ") || strings.Contains(string(log), " up ") {
		t.Fatal("同じSHAを再配置", string(log))
	}
}

func TestNASUpdateServerBuildKeepsEnvToolsImage(t *testing.T) {
	root, env := nasScripts(t, "old")
	nas := filepath.Join(root, "deploy", "nas")
	files := map[string]string{
		filepath.Join(root, "bin", "git"):                               "#!/bin/sh\ncase \" $* \" in *' symbolic-ref '*) echo trunk;; *' rev-parse '*) echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb;; esac\n",
		filepath.Join(nas, ".env"):                                      "ISSUE_DESK_TOOLS_IMAGE=issue-desk-tools:aaaaaaaaaaaa\n",
		filepath.Join(nas, "update-skills.sh"):                          "skills_listed(){ :; };skills_guard(){ :; }\n",
		filepath.Join(nas, "deploy-server.sh"):                          "#!/bin/sh\nexit 0\n",
		filepath.Join(root, "runtime", "worker-release", "tools-image"): "issue-desk-tools:bbbbbbbbbbbb\n",
	}
	for file, body := range files {
		if e := os.MkdirAll(filepath.Dir(file), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(file, []byte(body), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Remove(filepath.Join(nas, "update-desk-access.sh")); e != nil {
		t.Fatal(e)
	}
	code, body := runNASScript(t, root, env, "update.sh")
	if code != 0 {
		t.Fatal(code, body)
	}
	log, e := os.ReadFile(filepath.Join(root, "docker.log"))
	if e != nil || !strings.Contains(string(log), "build --build-arg TOOLS_IMAGE=issue-desk-tools:aaaaaaaaaaaa ") {
		t.Fatal(string(log), e)
	}
}

func TestNASHealthMatchingParsesTopLevelJSONAndExactRevisionRole(t *testing.T) {
	root, env := nasScripts(t, "same")
	raw, e := os.ReadFile(filepath.Join(root, "deploy", "nas", "deploy-server.sh"))
	if e != nil {
		t.Fatal(e)
	}
	function := nasDeployFunction(t, string(raw), "matches")
	target := strings.Repeat("b", 40)
	for _, item := range []struct {
		body, revision, role string
		ok                   bool
	}{{`{"db":{"ok":true},"ok":true,"revision":"` + target + `","role":"active"}`, target, "active", true}, {`{"role":"standby","revision":"` + target + `","ok":true}`, target, "any", true}, {`{"db":{"ok":true},"ok":false,"revision":"` + target + `","role":"active"}`, target, "active", false}, {`{"ok":true,"revision":"` + target + `","role":"standby"}`, target, "active", false}, {`{"ok":true,"revision":"wrong","role":"active"}`, target, "active", false}, {`{"ok":true`, "any", "any", false}} {
		command := exec.CommandContext(t.Context(), "sh", "-c", "READ_LIMIT=30\nbounded(){ shift; \"$@\"; }\n"+function+"\nmatches \"$1\" \"$2\" \"$3\"", "sh", item.body, item.revision, item.role)
		command.Env = env
		output, e := command.CombinedOutput()
		if (e == nil) != item.ok {
			t.Fatal(item, e, string(output))
		}
	}
}

func TestNASBlueGreenSuccessConfirmsNewWriterBeforeKeepingOldStopped(t *testing.T) {
	root, env := nasScripts(t, "native")
	env = append(env, "ISSUE_DESK_UPDATE_TIMEOUT=3")
	code, body := runNASScript(t, root, env, "deploy-server.sh", "--revision", strings.Repeat("b", 40))
	if code != 0 || !strings.Contains(body, "[server:success]") {
		log, _ := os.ReadFile(filepath.Join(root, "docker.log"))
		t.Fatal(code, body, string(log))
	}
	log, e := os.ReadFile(filepath.Join(root, "docker.log"))
	if e != nil || !strings.Contains(string(log), "kill --signal=SIGUSR2 blue") {
		t.Fatal("書き手へ通常の引継ぎを通知しない", string(log), e)
	}
	for file, want := range map[string]string{"blue": "false", "green": "true", "entry": "green", "owner": "green"} {
		raw, e := os.ReadFile(filepath.Join(root, "state", file))
		if e != nil || strings.TrimSpace(string(raw)) != want {
			t.Fatal(file, string(raw), e)
		}
	}
	saved, e := os.ReadFile(filepath.Join(root, "deploy", "nas", ".update-intake-last-success"))
	if e != nil || !strings.HasPrefix(string(saved), strings.Repeat("b", 40)+" ") {
		t.Fatal("成功した稼働版を記録しない", string(saved), e)
	}
}
