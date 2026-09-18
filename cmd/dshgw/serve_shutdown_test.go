package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type browserShutdownFake struct {
	quiesce  func()
	shutdown func(context.Context) error
}

func (f browserShutdownFake) Quiesce()                           { f.quiesce() }
func (f browserShutdownFake) Shutdown(ctx context.Context) error { return f.shutdown(ctx) }

func TestBrowserShutdownOrdersOwnersBeforeUnmount(t *testing.T) {
	var order []string
	done := make(chan struct{})
	close(done)
	service := browserShutdownFake{quiesce: func() { order = append(order, "disconnect") }, shutdown: func(context.Context) error { order = append(order, "unmount"); return nil }}
	err := shutdownBrowserWorkspaces(context.Background(), service, done, done,
		func(context.Context) error { order = append(order, "drain"); return nil },
		func(context.Context) error {
			// A StopWorker-style callback could reenter a service hook here. No service
			// lock is held by the coordinator (production uses runner.Shutdown instead).
			service.Quiesce()
			order = append(order, "stop")
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"disconnect", "drain", "disconnect", "stop", "unmount"}) {
		t.Fatal(order)
	}
}

func TestBrowserShutdownFailuresNeverPretendCleanup(t *testing.T) {
	sentinel := errors.New("failed")
	for _, stage := range []string{"drain", "stop", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			cleaned := false
			service := browserShutdownFake{quiesce: func() {}, shutdown: func(context.Context) error {
				cleaned = true
				if stage == "cleanup" {
					return sentinel
				}
				return nil
			}}
			err := shutdownBrowserWorkspaces(context.Background(), service, nil, nil,
				func(context.Context) error {
					if stage == "drain" {
						return sentinel
					}
					return nil
				},
				func(context.Context) error {
					if stage == "stop" {
						return sentinel
					}
					return nil
				})
			if !errors.Is(err, sentinel) {
				t.Fatal("failure hidden", err)
			}
			if cleaned != (stage == "cleanup") {
				t.Fatal("cleanup after failed prerequisite")
			}
		})
	}
}

func TestBrowserShutdownWaitsForStartupAndReaper(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	pending := make(chan struct{})
	service := browserShutdownFake{quiesce: func() {}, shutdown: func(context.Context) error { t.Error("cleanup before lifecycle owner exit"); return nil }}
	err := shutdownBrowserWorkspaces(ctx, service, pending, nil, func(context.Context) error { t.Error("drain before owner exit"); return nil }, func(context.Context) error { t.Error("stop before owner exit"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
