package startup

import (
	"sync"
	"time"
)

// reviewNotesAttempts is only an optimization; the database remains the authority.
type reviewNotesAttempts struct {
	mu    sync.Mutex
	dates map[string]string
}

func (a *reviewNotesAttempts) run(repo string, now func() time.Time, run func() (bool, error)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	date := now().In(time.FixedZone("JST", 9*60*60)).Format("2006-01-02")
	if a.dates[repo] >= date {
		return nil
	}
	attempted, err := run()
	// A claimed attempt remains claimed even if later work failed. A claim
	// failure must be retried. Avoid caching across a midnight wait.
	if (attempted || err == nil) && now().In(time.FixedZone("JST", 9*60*60)).Format("2006-01-02") == date {
		if a.dates == nil {
			a.dates = make(map[string]string)
		}
		a.dates[repo] = date
	}
	return err
}
