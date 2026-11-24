package main

import (
	"context"
	"os"
	"sync"
	"syscall"
)

// signalNotifications consumes an explicitly supplied signal stream. The caller
// owns registration and the input channel; no process signals are registered here.
type signalNotifications struct {
	Context context.Context
	Handoff <-chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

func newSignalNotifications(parent context.Context, input <-chan os.Signal) *signalNotifications {
	ctx, cancel := context.WithCancel(parent)
	handoff := make(chan struct{}, 1)
	s := &signalNotifications{Context: ctx, Handoff: handoff, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-input:
				if !ok {
					cancel()
					return
				}
				switch sig {
				case syscall.SIGUSR2:
					select {
					case handoff <- struct{}{}:
					default:
					}
				case syscall.SIGTERM, os.Interrupt:
					cancel()
					return
				}
			}
		}
	}()
	return s
}

// Close cancels the stop context and waits until the input consumer exits.
func (s *signalNotifications) Close() {
	s.once.Do(s.cancel)
	<-s.done
}
