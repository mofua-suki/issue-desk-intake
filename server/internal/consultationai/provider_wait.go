package consultationai

import (
	"context"
	"hash/fnv"
	"sync"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
)

// One Service owns one Runner and its account credentials per provider. Keep
// cooldowns local to that account boundary, never global across Services or
// providers. Consultation-specific waits remain durable in rateLimitWait.
type providerWaits struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (p *providerWaits) extend(provider string, until time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until == nil {
		p.until = make(map[string]time.Time)
	}
	if previous := p.until[provider]; previous.After(until) {
		return previous
	}
	p.until[provider] = until
	return until
}

func (p *providerWaits) deadline(provider string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.until[provider]
	if !until.After(now) {
		delete(p.until, provider)
		return time.Time{}
	}
	return until
}

// New calls share the observed rate-limit deadline. A bounded, stable spread
// avoids waking all consultations at the same instant. No AI attempt or usage
// is charged while waiting; callDynamic rechecks the existing budget afterwards.
func (s *Service) waitForProvider(ctx context.Context, d Record, provider string) error {
	until := s.rateWaits.deadline(provider, s.options.Now())
	if until.IsZero() {
		return nil
	}
	spread := providerWaitSpread(d)
	return consultations.WithoutAISlot(ctx, func(ctx context.Context) error {
		for {
			delay := until.Add(spread).Sub(s.options.Now())
			if delay <= 0 {
				return nil
			}
			if err := s.options.Wait(ctx, delay); err != nil {
				return err
			}
			next := s.rateWaits.deadline(provider, s.options.Now())
			if !next.After(until) {
				return nil
			}
			until = next
		}
	})
}

func providerWaitSpread(d Record) time.Duration {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(fleet.Text(d["id"])))
	return time.Duration(hash.Sum32()%2000) * time.Millisecond
}
