package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func awaitSignalTest(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
}

func sendSignalTest(t *testing.T, ch chan<- os.Signal, sig os.Signal) {
	t.Helper()
	select {
	case ch <- sig:
	case <-time.After(3 * time.Second):
		t.Fatal("signal input blocked")
	}
}

func closeSignalTest(t *testing.T, s *signalNotifications) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	awaitSignalTest(t, done)
}

func TestSignalNotificationsHandoff(t *testing.T) {
	input := make(chan os.Signal)
	s := newSignalNotifications(context.Background(), input)
	defer closeSignalTest(t, s)
	sendSignalTest(t, input, syscall.SIGUSR2)
	awaitSignalTest(t, s.Handoff)
	// Fill the notification buffer, then verify repeated inputs still get consumed.
	sendSignalTest(t, input, syscall.SIGUSR2)
	sendSignalTest(t, input, syscall.SIGUSR2)
	select {
	case <-s.Context.Done():
		t.Fatal("handoff stopped context")
	default:
	}
	sendSignalTest(t, input, syscall.SIGTERM)
	awaitSignalTest(t, s.Context.Done())
	if got := len(s.Handoff); got != 1 {
		t.Fatalf("handoff notifications = %d, want 1", got)
	}
	closeSignalTest(t, s)
}

func TestSignalNotificationsStop(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(sig.String(), func(t *testing.T) {
			input := make(chan os.Signal)
			s := newSignalNotifications(context.Background(), input)
			sendSignalTest(t, input, sig)
			awaitSignalTest(t, s.Context.Done())
			closeSignalTest(t, s)
		})
	}
	t.Run("parent", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		s := newSignalNotifications(parent, make(chan os.Signal))
		cancel()
		awaitSignalTest(t, s.Context.Done())
		closeSignalTest(t, s)
	})
	t.Run("close", func(t *testing.T) {
		s := newSignalNotifications(context.Background(), make(chan os.Signal))
		closeSignalTest(t, s)
		closeSignalTest(t, s)
		awaitSignalTest(t, s.Context.Done())
	})
}
