package contract

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunReportsRequiredFailureAndSkip(t *testing.T) {
	report := Run(context.Background(), []Check{{Name: "ok", Required: true, Run: func(context.Context) error { return nil }}, {Name: "optional", Run: func(context.Context) error { return ErrSkipped }}, {Name: "bad", Required: true, Timeout: time.Second, Run: func(context.Context) error { return errors.New("no") }}})
	if report.OK {
		t.Fatal("required failure not reflected")
	}
	if !report.Results[0].OK || !report.Results[1].Skipped || report.Results[2].Error != "no" {
		t.Fatalf("%#v", report)
	}
}
