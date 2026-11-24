package ai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeFakeProcess(t *testing.T, root string, pid, group int, command string, utime, stime int, io string, sockets ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	stat := strconv.Itoa(pid) + " (" + command + ") S 1 " + strconv.Itoa(group) + " 1 0 -1 0 0 0 0 0 " + strconv.Itoa(utime) + " " + strconv.Itoa(stime) + " 0 0 20 0 1 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if io != "" {
		if err := os.WriteFile(filepath.Join(dir, "io"), []byte(io), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, socket := range sockets {
		if err := os.Symlink(socket, filepath.Join(dir, "fd", strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
}

func requireValue(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", name, got, want)
	}
}

const fakeTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0035 0101A8C0:01BB 01 00000010:00000020 00:00000000 00000000  1000        0 111 1 0 0
   1: 0100007F:0036 0101A8C0:01BB 08 00000001:00000002 00:00000000 00000000  1000        0 222 1 0 0
   2: 0100007F:0037 0101A8C0:01BB 01 000000FF:000000FF 00:00000000 00000000  1000        0 999 1 0 0
`

func TestSampleProcessGroupCountsOnlyTheGroup(t *testing.T) {
	root := t.TempDir()
	writeFakeProcess(t, root, 50, 50, "co) de (x", 10, 5, "rchar: 100\nwchar: 7\n", "socket:[111]", "pipe:[5]")
	writeFakeProcess(t, root, 51, 50, "child", 2, 3, "rchar: 20\nwchar: 1\n", "socket:[222]")
	writeFakeProcess(t, root, 60, 60, "other", 90, 90, "rchar: 9000\nwchar: 9000\n", "socket:[999]")
	if err := os.MkdirAll(filepath.Join(root, "50", "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "50", "net", "tcp"), []byte(fakeTCP), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "self"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sample := sampleProcessGroup(root, 50, time.Now())
	requireValue(t, "processes", sample.processes, 2)
	requireValue(t, "children", sample.children, 1)
	requireValue(t, "cpuTicks", sample.cpuTicks, 20)
	requireValue(t, "readBytes", sample.readBytes, 120)
	requireValue(t, "writeBytes", sample.writeBytes, 8)
	requireValue(t, "established", sample.established, 1)
	requireValue(t, "other", sample.other, 1)
	requireValue(t, "txQueue", sample.txQueue, 0x11)
	requireValue(t, "rxQueue", sample.rxQueue, 0x22)
}

func TestSampleProcessGroupLeavesUnreadableValuesUnknown(t *testing.T) {
	root := t.TempDir()
	if sample := sampleProcessGroup(filepath.Join(root, "missing"), 50, time.Now()); sample.processes != nil {
		t.Fatalf("no proc: %+v", sample)
	}
	if sample := sampleProcessGroup(root, 50, time.Now()); sample.processes != nil {
		t.Fatalf("no such group: %+v", sample)
	}
	// io と net が読めないプロセスでは、読めた分だけを 0 に見せず不明にする。
	writeFakeProcess(t, root, 50, 50, "ai", 1, 1, "", "socket:[111]")
	writeFakeProcess(t, root, 51, 50, "child", 1, 1, "rchar: 20\nwchar: 1\n")
	sample := sampleProcessGroup(root, 50, time.Now())
	requireValue(t, "processes", sample.processes, 2)
	if sample.readBytes != nil || sample.writeBytes != nil {
		t.Fatalf("io of one process is missing: %v %v", sample.readBytes, sample.writeBytes)
	}
	if sample.established != nil || sample.txQueue != nil {
		t.Fatalf("connections are unreadable: %v %v", sample.established, sample.txQueue)
	}
	// fd が読めないときも、接続は 0 件ではなく不明にする。
	if err := os.RemoveAll(filepath.Join(root, "51", "fd")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "50", "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "50", "net", "tcp"), []byte(fakeTCP), 0o644); err != nil {
		t.Fatal(err)
	}
	if sample := sampleProcessGroup(root, 50, time.Now()); sample.established != nil {
		t.Fatalf("fd is unreadable: %v", *sample.established)
	}
}

func TestProcShowsThisProcessRejectsAnotherNamespace(t *testing.T) {
	root := t.TempDir()
	if procShowsThisProcess(root) {
		t.Fatal("no self link")
	}
	if err := os.Symlink(strconv.Itoa(os.Getpid()+1), filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	if procShowsThisProcess(root) {
		t.Fatal("self is another number")
	}
	if err := os.Remove(filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strconv.Itoa(os.Getpid()), filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	if !procShowsThisProcess(root) {
		t.Fatal("self is this process")
	}
}

func TestNewIdleSnapshotComparesWithTheBaseline(t *testing.T) {
	value := func(v int64) *int64 { return &v }
	at := time.Now()
	current := groupSample{at: at, processes: value(2), children: value(1), cpuTicks: value(150), readBytes: value(500), writeBytes: value(9), established: value(1)}
	baseline := groupSample{at: at.Add(-3 * time.Second), cpuTicks: value(100), readBytes: value(600), writeBytes: value(4)}
	snapshot := newIdleSnapshot(current, &baseline)
	requireValue(t, "cpuMs", snapshot.CPUMs, 1500)
	requireValue(t, "windowMs", snapshot.WindowMs, 3000)
	requireValue(t, "cpuDeltaMs", snapshot.CPUDeltaMs, 500)
	requireValue(t, "writeBytesDelta", snapshot.WriteBytesDelta, 5)
	if snapshot.ReadBytesDelta != nil {
		t.Fatalf("a shrinking total is unknown, got %d", *snapshot.ReadBytesDelta)
	}
	early := newIdleSnapshot(current, nil)
	if early.WindowMs != nil || early.CPUDeltaMs != nil || early.ReadBytesDelta != nil {
		t.Fatalf("without a baseline the window is unknown: %+v", early)
	}
	requireValue(t, "cpuMs without baseline", early.CPUMs, 1500)
}

func TestRunRecordsTheProcessGroupOnIdleTimeout(t *testing.T) {
	// 別のPID名前空間で動くテスト環境では、/proc に見える番号と起動したプロセスの番号が合わず、値は不明になる。
	// その環境でも不明のまま記録できることを確かめ、読める環境でだけ値を比べる。
	if sampleStartedGroup(syscall.Getpgrp()).processes == nil {
		unknown, err := runShell(t, context.Background(), "sleep 5", 20*time.Second, 300*time.Millisecond)
		if err == nil || unknown.Cause != "timeout_idle" || unknown.Idle == nil || unknown.Idle.Processes != nil || unknown.Idle.CPUMs != nil {
			t.Fatalf("unknown: cause=%q idle=%+v err=%v", unknown.Cause, unknown.Idle, err)
		}
		t.Skip("/proc からこのプロセスのグループを読めない環境")
	}
	quiet, err := runShell(t, context.Background(), "(sleep 5) & wait", 20*time.Second, 800*time.Millisecond)
	if err == nil || quiet.Cause != "timeout_idle" || quiet.Idle == nil || quiet.Idle.CPUDeltaMs == nil || *quiet.Idle.CPUDeltaMs > 100 {
		t.Fatalf("quiet: %+v err=%v", quiet.Idle, err)
	}
	if quiet.Idle.Processes == nil || *quiet.Idle.Processes < 2 || quiet.Idle.Children == nil || *quiet.Idle.Children < 1 ||
		quiet.Idle.Established == nil || *quiet.Idle.Established != 0 || quiet.Idle.CPUMs == nil || quiet.Idle.WindowMs == nil {
		t.Fatalf("incomplete group snapshot: %+v", quiet.Idle)
	}
}

func TestRunRecordsNoSnapshotForOtherEnds(t *testing.T) {
	total, err := runShell(t, context.Background(), "while true; do echo tick; sleep 0.05; done", 400*time.Millisecond, 20*time.Second)
	if err == nil || total.Cause != "timeout_total" || total.Idle != nil {
		t.Fatalf("total: cause=%q idle=%+v", total.Cause, total.Idle)
	}
	done, err := runShell(t, context.Background(), "echo ok", 10*time.Second, 10*time.Second)
	if err != nil || done.Idle != nil {
		t.Fatalf("done: idle=%+v err=%v", done.Idle, err)
	}
}

func TestDiagnosticKeepsTheSnapshotAsNumbersOnly(t *testing.T) {
	request, _ := diagnosticFixture(t)
	value := int64(3)
	runner := Runner{Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		if len(r.Arguments) > 0 && r.Arguments[0] == "login" {
			return ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		return ProcessResult{Stdout: []byte(`{"type":"turn.started","secret":"SECRET-ANSWER"}` + "\n"), Stderr: []byte("SECRET-STDERR"), Cause: "timeout_idle", Idle: &IdleSnapshot{Processes: &value}, FirstOutput: -1, LastOutput: -1}, failed("timeout", "x", true)
	}}
	answer, err := runner.Call(context.Background(), request)
	if err == nil {
		t.Fatal("want a timeout")
	}
	stored, _ := answer.Diagnostic.Map()["idleProcess"].(map[string]any)
	if stored == nil || stored["processes"] != float64(3) {
		t.Fatalf("idleProcess = %v", answer.Diagnostic.Map()["idleProcess"])
	}
	if value, present := stored["cpuMs"]; !present || value != nil {
		t.Fatalf("an unknown value stays null: %v %v", value, present)
	}
	for key, item := range stored {
		if _, isNumber := item.(float64); item != nil && !isNumber {
			t.Fatalf("%s holds %T", key, item)
		}
	}
	raw, err := json.Marshal(answer.Diagnostic.Map())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "SECRET-ANSWER", "SECRET-STDERR"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("diagnostic leaks %q", secret)
		}
	}
	if _, present := (Diagnostic{}).Map()["idleProcess"]; present {
		t.Fatal("other kinds do not carry idleProcess")
	}
}

func TestRunProcessActivityAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  string
		total time.Duration
		want  string
	}{
		{"read continues", "read", 650 * time.Millisecond, "timeout_total"},
		{"cpu continues", "cpu", 650 * time.Millisecond, "timeout_total"},
		{"activity stops", "stops", 2 * time.Second, "timeout_idle"},
		{"proc unavailable", "missing", 2 * time.Second, "timeout_idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			sample := func(_ int) groupSample {
				elapsed := time.Since(start)
				n := int64(elapsed / (20 * time.Millisecond))
				zero := int64(0)
				switch tc.mode {
				case "read":
					return groupSample{at: time.Now(), readBytes: &n, cpuTicks: &zero}
				case "cpu":
					return groupSample{at: time.Now(), cpuTicks: &n, readBytes: &zero}
				case "stops":
					if elapsed > 350*time.Millisecond {
						n = 17
					}
					return groupSample{at: time.Now(), readBytes: &n, cpuTicks: &zero}
				default:
					return groupSample{at: time.Now()}
				}
			}
			result, err := runWithSampler(context.Background(), ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", "sleep 5"}, Directory: t.TempDir(), Timeout: tc.total, IdleTimeout: 200 * time.Millisecond}, sample)
			if err == nil || result.Cause != tc.want {
				t.Fatalf("cause=%s err=%v", result.Cause, err)
			}
			if tc.mode == "stops" && time.Since(start) < 400*time.Millisecond {
				t.Fatal("activity did not extend idle timeout")
			}
		})
	}
}

func TestGroupMadeProgressUnknownAndDecreasing(t *testing.T) {
	value := func(v int64) *int64 { return &v }
	for _, pair := range [][2]groupSample{
		{{readBytes: value(1)}, {}}, {{}, {cpuTicks: value(5)}},
		{{readBytes: value(5), cpuTicks: value(5)}, {readBytes: value(4), cpuTicks: value(5)}},
	} {
		if groupMadeProgress(pair[0], pair[1]) {
			t.Fatal("unknown or decreasing counters must not be progress")
		}
	}
}

func TestRunCPUActivityReachesTotalTimeout(t *testing.T) {
	if sampleStartedGroup(syscall.Getpgrp()).processes == nil {
		t.Skip("proc unavailable")
	}
	result, err := runShell(t, context.Background(), "while :; do :; done", 700*time.Millisecond, 200*time.Millisecond)
	if err == nil || result.Cause != "timeout_total" {
		t.Fatalf("cause=%q err=%v", result.Cause, err)
	}
}

func TestStatusUsesRunForAuthentication(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "provider")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'Logged in using ChatGPT'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	status := (&Runner{Codex: exe}).Status(context.Background(), "codex", dir)
	if !status.Ready || status.Reason != "ready" {
		t.Fatalf("status=%+v", status)
	}
}
