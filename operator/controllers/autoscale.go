package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// prometheusURL points at the kube-prometheus-stack installed via GitOps
// (gitops/infrastructure/monitoring). Configurable for tests.
var (
	prometheusURL        = getenvDefault("PROMETHEUS_URL", "http://monitoring-kube-prometheus-prometheus.monitoring.svc:9090")
	prometheusHTTPClient = &http.Client{Timeout: 5 * time.Second}
)

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// consumerLag sums the lag (end offset - committed offset) across all
// partitions of the consumer group. Creates and closes an ephemeral Kafka
// client per evaluation — acceptable at the scale of a handful of
// pipelines per reconcile.
func consumerLag(ctx context.Context, brokers []string, group, topic string) (int64, error) {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return 0, fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)

	offsets, err := adm.FetchOffsets(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("fetch offsets: %w", err)
	}
	endOffsets, err := adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return 0, fmt.Errorf("list end offsets: %w", err)
	}

	var lag int64
	offsets.Each(func(o kadm.OffsetResponse) {
		end, ok := endOffsets.Lookup(o.Topic, o.Partition)
		if !ok {
			return
		}
		if l := end.Offset - o.At; l > 0 {
			lag += l
		}
	})
	return lag, nil
}

// desiredReplicasFromLag rounds up: lag/targetLagPerReplica.
func desiredReplicasFromLag(lag int64, targetLagPerReplica int64, min, max int32) int32 {
	if targetLagPerReplica <= 0 {
		targetLagPerReplica = 1000
	}
	d := int32((lag + targetLagPerReplica - 1) / targetLagPerReplica)
	if d < min {
		d = min
	}
	if d > max {
		d = max
	}
	return d
}

// sloLatencyViolated queries Prometheus for the pipeline's p99.9; returns
// false (no violation) on error/no data, so a temporary metrics outage
// doesn't block the reconcile.
func sloLatencyViolated(ctx context.Context, pipeline string, maxMs int32) bool {
	q := fmt.Sprintf(`histogram_quantile(0.999, sum(rate(zeedfai_scorer_latency_seconds_bucket{pipeline=%q}[2m])) by (le))`, pipeline)
	u := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(prometheusURL, "/"), url.QueryEscape(q))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := prometheusHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var body struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Data.Result) == 0 {
		return false
	}
	s, ok := body.Data.Result[0].Value[1].(string)
	if !ok {
		return false
	}
	seconds, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return false
	}
	return seconds*1000 > float64(maxMs)
}

func cooldownElapsed(last *time.Time, cooldown time.Duration) bool {
	return last == nil || time.Since(*last) >= cooldown
}
