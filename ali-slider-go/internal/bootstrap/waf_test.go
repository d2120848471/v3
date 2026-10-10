package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
)

func TestNewWAFExecutorDefersRuntimeAndNetworkWork(t *testing.T) {
	for _, timeout := range []time.Duration{500 * time.Microsecond, 25 * time.Second, 5 * time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			executor, err := NewWAFExecutor("/missing-v8-library", timeout)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = executor.SolveWAF(ctx, service.WAFRequest{PageURL: "https://page.example.com/"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled execution error=%v", err)
			}
		})
	}
}

func TestNewWAFExecutorRejectsInvalidServerConfiguration(t *testing.T) {
	for _, test := range []struct {
		library string
		timeout time.Duration
	}{{"", time.Second}, {"/libv8", -time.Second}} {
		if _, err := NewWAFExecutor(test.library, test.timeout); err == nil {
			t.Fatal("invalid server configuration was accepted")
		}
	}
}
