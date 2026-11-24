package operations

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// RuntimeMeasurements measures the Go timer's actual wake-up delay and a database
// clock read. PostgreSQL clock offset is bounded by the request's two timestamps;
// this is not Node event-loop utilization or a claim of clock synchronization.
type RuntimeMeasurements struct {
	delayClearStreak                               int
	window                                         runtimeDelayWindow
	delayStreak                                    int
	delayLastOver, delayAlertAt                    time.Time
	delayAlertMax                                  float64
	db                                             *sql.DB
	now                                            func() time.Time
	mu                                             sync.Mutex
	started, ended                                 time.Time
	samples, dbSamples, dbFailures                 int
	delaySum, delayMax, roundTripSum, roundTripMax float64
	offsetLow, offsetHigh                          *float64
}

func NewRuntimeMeasurements(db *sql.DB, now func() time.Time) *RuntimeMeasurements {
	if now == nil {
		now = time.Now
	}
	return &RuntimeMeasurements{db: db, now: now}
}
func (m *RuntimeMeasurements) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	expected := time.Now().Add(interval)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			actual := time.Now()
			delay := actual.Sub(expected)
			if delay < 0 {
				delay = 0
			}
			m.observeDelay(m.now(), float64(delay)/float64(time.Millisecond))
			if m.db != nil {
				probe, done := context.WithTimeout(ctx, min(interval, 5*time.Second))
				before := m.now()
				monotonic := time.Now()
				var clock time.Time
				e := m.db.QueryRowContext(probe, "SELECT clock_timestamp()").Scan(&clock)
				elapsed := time.Since(monotonic)
				after := m.now()
				done()
				m.observeDB(before, after, clock, float64(elapsed)/float64(time.Millisecond), e)
			}
			expected = time.Now().Add(interval)
			timer.Reset(interval)
		}
	}
}
func (m *RuntimeMeasurements) observeDelay(at time.Time, delay float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() {
		m.started = at
	}
	m.ended = at
	m.samples++
	m.observeWindow(at, delay)
	m.delaySum += delay
	m.delayMax = max(m.delayMax, delay)
}
func (m *RuntimeMeasurements) observeDB(before, after, clock time.Time, elapsed float64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.dbFailures++
		return
	}
	m.dbSamples++
	m.roundTripSum += elapsed
	m.roundTripMax = max(m.roundTripMax, elapsed)
	low, high := float64(clock.Sub(after))/float64(time.Millisecond), float64(clock.Sub(before))/float64(time.Millisecond)
	m.offsetLow, m.offsetHigh = &low, &high
}
func (m *RuntimeMeasurements) View() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]any{"source": "go-runtime", "from": nil, "to": nil, "samples": m.samples, "timerDelayMsMean": nil, "timerDelayMsMax": nil, "dbSamples": m.dbSamples, "dbFailures": m.dbFailures, "dbReason": nil, "dbRoundTripMsMean": nil, "dbRoundTripMsMax": nil, "dbClockOffsetMsLower": m.offsetLow, "dbClockOffsetMsUpper": m.offsetHigh, "reason": nil}
	if m.samples == 0 {
		out["reason"] = "Goの周期計測はまだ届いていません"
	} else {
		out["from"], out["to"] = iso(m.started), iso(m.ended)
		out["timerDelayMsMean"], out["timerDelayMsMax"] = m.delaySum/float64(m.samples), m.delayMax
	}
	if m.db == nil {
		out["dbReason"] = "共有DBの接続がありません"
	} else if m.dbSamples == 0 {
		out["dbReason"] = "成功したDB時計の計測がありません"
	}

	if m.dbSamples > 0 {
		out["dbRoundTripMsMean"], out["dbRoundTripMsMax"] = m.roundTripSum/float64(m.dbSamples), m.roundTripMax
	}
	return out
}
