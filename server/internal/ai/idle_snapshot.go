package ai

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IdleSnapshot records what the provider's process group was doing when it was killed for
// staying silent. It tells a process that is working or receiving from one that is blocked.
// It holds counts only: never a command line, a path, an address, a prompt or an answer.
// A value that could not be read is nil, never 0.
type IdleSnapshot struct {
	Processes        *int64 // processes in the group, the provider itself included
	Children         *int64 // the same without the provider itself
	CPUMs            *int64 // CPU time the group has used so far
	WindowMs         *int64 // from the sample after the last output to the kill
	CPUDeltaMs       *int64 // CPU time used during that window
	ReadBytesDelta   *int64 // bytes the group read during that window (rchar: sockets and files)
	WriteBytesDelta  *int64 // bytes the group wrote during that window (wchar)
	Established      *int64 // established TCP connections the group holds
	OtherConnections *int64 // TCP connections in any other state
	TxQueueBytes     *int64 // bytes the peer has not acknowledged yet
	RxQueueBytes     *int64 // bytes received but not read by the process yet
}

// Map is the persisted form (JSON-friendly). Unknown values are nil, never 0.
func (s IdleSnapshot) Map() map[string]any {
	number := func(v *int64) any {
		if v == nil {
			return nil
		}
		return float64(*v)
	}
	return map[string]any{
		"processes": number(s.Processes), "children": number(s.Children), "cpuMs": number(s.CPUMs),
		"windowMs": number(s.WindowMs), "cpuDeltaMs": number(s.CPUDeltaMs),
		"readBytesDelta": number(s.ReadBytesDelta), "writeBytesDelta": number(s.WriteBytesDelta),
		"established": number(s.Established), "otherConnections": number(s.OtherConnections),
		"txQueueBytes": number(s.TxQueueBytes), "rxQueueBytes": number(s.RxQueueBytes),
	}
}

// groupSample is one reading of a process group. Cumulative values are compared between readings.
type groupSample struct {
	at                                   time.Time
	processes, children, cpuTicks        *int64
	readBytes, writeBytes                *int64
	established, other, txQueue, rxQueue *int64
}

// userHertz is the unit of utime and stime in /proc/<pid>/stat. Linux fixes it at 100 for user space.
const userHertz = 100

const procRoot = "/proc"

// procShowsThisProcess reports whether root numbers processes as this process sees them. In another
// PID namespace the same number can belong to an unrelated process, so nothing is read from such a root.
func procShowsThisProcess(root string) bool {
	self, err := os.Readlink(filepath.Join(root, "self"))
	return err == nil && self == strconv.Itoa(os.Getpid())
}

// sampleStartedGroup reads the group of a process this process has started.
func sampleStartedGroup(group int) groupSample {
	at := time.Now()
	if !procShowsThisProcess(procRoot) {
		return groupSample{at: at}
	}
	return sampleProcessGroup(procRoot, group, at)
}

// sampleProcessGroup reads root (normally /proc). A process that ends while being read is skipped.
func sampleProcessGroup(root string, group int, at time.Time) groupSample {
	sample := groupSample{at: at}
	entries, err := os.ReadDir(root)
	if err != nil {
		return sample
	}
	var processes, ticks, reads, writes int64
	leader, ioKnown, socketsKnown := false, true, true
	sockets := map[string]bool{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		processGroup, used, ok := readStat(filepath.Join(dir, "stat"))
		if !ok || processGroup != group {
			continue
		}
		processes++
		ticks += used
		leader = leader || pid == group
		read, written, ok := readIO(filepath.Join(dir, "io"))
		ioKnown = ioKnown && ok
		reads, writes = reads+read, writes+written
		socketsKnown = readSockets(filepath.Join(dir, "fd"), sockets) && socketsKnown
	}
	if processes == 0 {
		return sample
	}
	cpu, children := ticks, processes
	if leader {
		children--
	}
	sample.processes, sample.children, sample.cpuTicks = &processes, &children, &cpu
	if ioKnown {
		sample.readBytes, sample.writeBytes = &reads, &writes
	}
	if socketsKnown {
		established, other, tx, rx, ok := readConnections(filepath.Join(root, strconv.Itoa(group), "net"), sockets)
		if ok {
			sample.established, sample.other, sample.txQueue, sample.rxQueue = &established, &other, &tx, &rx
		}
	}
	return sample
}

// readStat returns the process group and the CPU ticks. The command name may hold spaces and
// parentheses, so the fields are counted from its last closing parenthesis.
func readStat(path string) (group int, ticks int64, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	text := string(raw)
	fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
	if len(fields) < 13 {
		return 0, 0, false
	}
	group, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	user, userErr := strconv.ParseInt(fields[11], 10, 64)
	system, systemErr := strconv.ParseInt(fields[12], 10, 64)
	if userErr != nil || systemErr != nil {
		return 0, 0, false
	}
	return group, user + system, true
}

func readIO(path string) (read, written int64, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	found := 0
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, _ := strings.Cut(line, ":")
		number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "rchar":
			read, found = number, found+1
		case "wchar":
			written, found = number, found+1
		}
	}
	return read, written, found == 2
}

func readSockets(dir string, sockets map[string]bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err == nil && strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			sockets[target[len("socket:["):len(target)-1]] = true
		}
	}
	return true
}

// readConnections counts the group's TCP sockets. The tables also list every other process's
// sockets, and addresses are never kept. One readable table is enough: IPv6 can be disabled.
func readConnections(dir string, sockets map[string]bool) (established, other, tx, rx int64, ok bool) {
	for _, name := range []string{"tcp", "tcp6"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		ok = true
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || !sockets[fields[9]] {
				continue
			}
			if fields[3] == "01" {
				established++
			} else {
				other++
			}
			sent, received, _ := strings.Cut(fields[4], ":")
			if value, err := strconv.ParseInt(sent, 16, 64); err == nil {
				tx += value
			}
			if value, err := strconv.ParseInt(received, 16, 64); err == nil {
				rx += value
			}
		}
	}
	return established, other, tx, rx, ok
}

// newIdleSnapshot turns the reading at the kill into a snapshot. baseline is the reading after the
// last output, or nil when none was taken yet. A cumulative value drops when a child has ended
// during the window, and then the delta is unknown.
func newIdleSnapshot(current groupSample, baseline *groupSample) *IdleSnapshot {
	snapshot := &IdleSnapshot{Processes: current.processes, Children: current.children,
		Established: current.established, OtherConnections: current.other, TxQueueBytes: current.txQueue, RxQueueBytes: current.rxQueue}
	if current.cpuTicks != nil {
		snapshot.CPUMs = ticksToMillis(*current.cpuTicks)
	}
	if baseline == nil {
		return snapshot
	}
	window := current.at.Sub(baseline.at).Milliseconds()
	snapshot.WindowMs = &window
	if delta := difference(current.cpuTicks, baseline.cpuTicks); delta != nil {
		snapshot.CPUDeltaMs = ticksToMillis(*delta)
	}
	snapshot.ReadBytesDelta = difference(current.readBytes, baseline.readBytes)
	snapshot.WriteBytesDelta = difference(current.writeBytes, baseline.writeBytes)
	return snapshot
}

func ticksToMillis(ticks int64) *int64 {
	value := ticks * 1000 / userHertz
	return &value
}

func difference(current, baseline *int64) *int64 {
	if current == nil || baseline == nil || *current < *baseline {
		return nil
	}
	value := *current - *baseline
	return &value
}

// idleSampleInterval is how often Run takes the baseline after an output.
func idleSampleInterval(idle time.Duration) time.Duration {
	if interval := idle / 4; interval > 0 && interval < 5*time.Second {
		return interval
	}
	return 5 * time.Second
}

// groupMadeProgress only compares available, nondecreasing counters.
func groupMadeProgress(previous, current groupSample) bool {
	for _, pair := range [][2]*int64{{previous.cpuTicks, current.cpuTicks}, {previous.readBytes, current.readBytes}} {
		if delta := difference(pair[1], pair[0]); delta != nil && *delta > 0 {
			return true
		}
	}
	return false
}
