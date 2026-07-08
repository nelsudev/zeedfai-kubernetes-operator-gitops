package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDesiredReplicasFromLag(t *testing.T) {
	tests := []struct {
		name   string
		lag    int64
		target int64
		min    int32
		max    int32
		want   int32
	}{
		{name: "zero lag keeps min", lag: 0, target: 1000, min: 2, max: 10, want: 2},
		{name: "rounds up", lag: 1001, target: 1000, min: 1, max: 10, want: 2},
		{name: "clamps max", lag: 50000, target: 1000, min: 1, max: 10, want: 10},
		{name: "defaults invalid target", lag: 2500, target: 0, min: 1, max: 10, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := desiredReplicasFromLag(tt.lag, tt.target, tt.min, tt.max); got != tt.want {
				t.Fatalf("desiredReplicasFromLag() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSloLatencyViolated(t *testing.T) {
	withPrometheusResponse(t, `{"data":{"result":[{"value":[0,"0.251"]}]}}`, func() {
		if !sloLatencyViolated(context.Background(), "card-payments-eu", 250) {
			t.Fatal("expected SLO violation")
		}
	})
}

func TestSloLatencyViolatedFailsOpenOnBadResponse(t *testing.T) {
	withPrometheusResponse(t, `{"data":{"result":[]}}`, func() {
		if sloLatencyViolated(context.Background(), "card-payments-eu", 250) {
			t.Fatal("expected no SLO violation when Prometheus has no data")
		}
	})
}

func TestCanaryErrorRatePct(t *testing.T) {
	withPrometheusResponse(t, `{"data":{"result":[{"value":[0,"12.5"]}]}}`, func() {
		got, ok := canaryErrorRatePct(context.Background(), "card-payments-eu")
		if !ok {
			t.Fatal("expected canary error rate")
		}
		if got != 12.5 {
			t.Fatalf("canaryErrorRatePct() = %v, want 12.5", got)
		}
	})
}

func TestCooldownElapsed(t *testing.T) {
	if !cooldownElapsed(nil, time.Minute) {
		t.Fatal("nil last scale should allow scaling")
	}
	recent := time.Now().Add(-10 * time.Second)
	if cooldownElapsed(&recent, time.Minute) {
		t.Fatal("recent scale should still be in cooldown")
	}
	old := time.Now().Add(-2 * time.Minute)
	if !cooldownElapsed(&old, time.Minute) {
		t.Fatal("old scale should allow scaling")
	}
}

func withPrometheusResponse(t *testing.T, body string, fn func()) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	oldURL := prometheusURL
	oldClient := prometheusHTTPClient
	prometheusURL = ts.URL
	prometheusHTTPClient = ts.Client()
	defer func() {
		prometheusURL = oldURL
		prometheusHTTPClient = oldClient
	}()

	fn()
}
