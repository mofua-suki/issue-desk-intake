package adminapi

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Observe actual state-file opens and atomic replacements. Include close events
// so successive identical open events cannot be coalesced by inotify.
func watchPushState(t *testing.T, s *service) func() (reads, updates int) {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	file, err := s.pushFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = syscall.InotifyAddWatch(fd, filepath.Dir(file), syscall.IN_OPEN|syscall.IN_CLOSE_NOWRITE|syscall.IN_MOVED_TO); err != nil {
		t.Fatal(err)
	}
	return func() (reads, updates int) {
		buffer := make([]byte, 64*1024)
		for {
			n, err := syscall.Read(fd, buffer)
			if errors.Is(err, syscall.EAGAIN) {
				return reads, updates
			}
			if err != nil {
				t.Fatal(err)
			}
			for offset := 0; offset < n; {
				mask := binary.NativeEndian.Uint32(buffer[offset+4:])
				length := int(binary.NativeEndian.Uint32(buffer[offset+12:]))
				name := buffer[offset+16 : offset+16+length]
				if mask&syscall.IN_Q_OVERFLOW != 0 {
					t.Fatal("state observation queue overflow")
				}
				if len(name) > len(filepath.Base(file)) && string(name[:len(filepath.Base(file))]) == filepath.Base(file) && name[len(filepath.Base(file))] == 0 {
					if mask&syscall.IN_OPEN != 0 {
						reads++
					}
					if mask&syscall.IN_MOVED_TO != 0 {
						updates++
					}
				}
				offset += 16 + length
			}
		}
	}
}

func TestPushPerformanceComparison(t *testing.T) {
	t.Logf("environment=%s %s/%s GOMAXPROCS=%d", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))
	// Same persisted keys/subscriptions and fake latency schedule in each run.
	// 32 subscriptions: 8 slow successes (20ms), 8 fast successes (2ms),
	// 8 gone (2ms), 8 timeouts (40ms). No external HTTP or PostgreSQL.
	s, _ := persistedPushFixture(t, 32)
	file, err := s.pushFile()
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	collect := watchPushState(t, s)
	var sequentialTime, parallelTime time.Duration
	for _, mode := range []struct {
		name    string
		send    func(context.Context, PhoneNotice) PhonePushResult
		maximum int32
	}{{"sequential-fixed-c1d1b9b", s.notifyPhoneSequentialBaseline, 1}, {"parallel-4", s.notifyPhone, 4}} {
		var elapsed time.Duration
		var allocated, allocations uint64
		for sample := 0; sample < 3; sample++ {
			if err = os.WriteFile(file, initial, 0600); err != nil {
				t.Fatal(err)
			}
			collect() // Exclude setup I/O.
			var active, maximum, calls, closed atomic.Int32
			s.HTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				n := active.Add(1)
				for old := maximum.Load(); n > old; old = maximum.Load() {
					if maximum.CompareAndSwap(old, n) {
						break
					}
				}
				defer active.Add(-1)
				index := pushIndex(r) % 4
				if index == 3 {
					ctx, cancel := context.WithTimeout(r.Context(), 40*time.Millisecond)
					defer cancel()
					<-ctx.Done()
					return nil, ctx.Err()
				}
				delay := 2 * time.Millisecond
				if index == 0 {
					delay = 20 * time.Millisecond
				}
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				response := pushResponse(201)
				if index == 2 {
					response.StatusCode = 410
				}
				response.Body = &countedPushBody{ReadCloser: response.Body, closed: &closed}
				return response, nil
			})}
			ctx := pushTestContext(t)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			result := mode.send(ctx, parallelNotice)
			elapsed += time.Since(start)
			runtime.ReadMemStats(&after)
			allocated += after.TotalAlloc - before.TotalAlloc
			allocations += after.Mallocs - before.Mallocs
			reads, updates := collect()
			if result != (PhonePushResult{Sent: 16, Failed: 16, Subscriptions: 32}) || calls.Load() != 32 || active.Load() != 0 || maximum.Load() != mode.maximum || closed.Load() != 24 {
				t.Fatalf("%s result=%+v calls=%d active=%d max=%d closed=%d", mode.name, result, calls.Load(), active.Load(), maximum.Load(), closed.Load())
			}
			if reads != 9 || updates != 8 {
				t.Fatalf("%s state reads=%d updates=%d", mode.name, reads, updates)
			}
			if remaining := len(savedPushSubscriptions(t, s)); remaining != 24 {
				t.Fatalf("remaining=%d", remaining)
			}
		}
		elapsed /= 3
		t.Logf("%s samples=3 mean=%s calls/op=32 state-reads/op=9 state-updates/op=8 B/op=%d allocs/op=%d max=%d final-subscriptions=24", mode.name, elapsed, allocated/3, allocations/3, mode.maximum)
		if mode.maximum == 1 {
			sequentialTime = elapsed
		} else {
			parallelTime = elapsed
		}
	}
	// Timings are reported, never used as a flaky pass/fail threshold.
	t.Logf("elapsed reduction=%.1f%% (synthetic 32 destinations, 20ms/2ms/410-2ms/40ms-timeout, 3 samples; includes crypto and durable state I/O)", 100*(1-float64(parallelTime)/float64(sequentialTime)))
}

// The response must be closed before the caller returns, including parallel sends.
type countedPushBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b *countedPushBody) Close() error { b.closed.Add(1); return b.ReadCloser.Close() }
