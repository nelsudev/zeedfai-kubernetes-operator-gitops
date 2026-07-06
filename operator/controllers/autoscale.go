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

// prometheusURL aponta para o kube-prometheus-stack instalado via GitOps
// (gitops/infrastructure/monitoring). Configurável para testes.
var prometheusURL = getenvDefault("PROMETHEUS_URL", "http://monitoring-kube-prometheus-prometheus.monitoring.svc:9090")

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// consumerLag soma o lag (end offset - committed offset) de todas as
// partições do grupo consumidor. Cria e fecha um cliente Kafka efémero por
// avaliação — aceitável à escala de um punhado de pipelines por reconcile.
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

// desiredReplicasFromLag arredonda para cima: lag/targetLagPerReplica.
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

// sloLatencyViolated consulta o Prometheus para a p99.9 do pipeline; retorna
// false (sem violação) em caso de erro/sem dados, para não bloquear o
// reconcile por indisponibilidade temporária de métricas.
func sloLatencyViolated(ctx context.Context, pipeline string, maxMs int32) bool {
	q := fmt.Sprintf(`histogram_quantile(0.999, sum(rate(zeedfai_scorer_latency_seconds_bucket{pipeline=%q}[2m])) by (le))`, pipeline)
	u := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(prometheusURL, "/"), url.QueryEscape(q))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
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
