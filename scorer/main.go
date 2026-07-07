// zeedfai scorer: consome transações do Kafka, aplica uma regra de scoring
// e expõe métricas Prometheus. O SLO alvo é p99.9 < 250ms (ver docs/ADR-0001).
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"math/rand"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/twmb/franz-go/pkg/kgo"
)

// registry e métricas são criados em main(), rotulados com PIPELINE_NAME,
// para que o PrometheusRule do operator os possa filtrar por pipeline.
var (
	processed prometheus.Counter
	flagged   prometheus.Counter
	errors    prometheus.Counter
	latency   prometheus.Histogram
)

type Transaction struct {
	ID        string  `json:"id"`
	Card      string  `json:"card"`
	AmountEUR float64 `json:"amount_eur"`
	Country   string  `json:"country"`
	Timestamp int64   `json:"ts"`
}

// score aplica a regra dummy: montante alto + país de risco = suspeito.
// Numa fase posterior isto seria uma chamada a um modelo.
func score(t Transaction) bool {
	risky := map[string]bool{"XX": true, "ZZ": true}
	return t.AmountEUR > 900 || (t.AmountEUR > 300 && risky[t.Country])
}

func main() {
	brokers := getenv("KAFKA_BROKERS", "localhost:9092")
	topic := getenv("KAFKA_TOPIC", "transactions")
	group := getenv("KAFKA_GROUP", "zeedfai-scorer")
	pipeline := getenv("PIPELINE_NAME", group)
	role := getenv("ROLE", "stable")

	reg := prometheus.WrapRegistererWith(prometheus.Labels{"pipeline": pipeline, "role": role}, prometheus.DefaultRegisterer)
	factory := promauto.With(reg)
	processed = factory.NewCounter(prometheus.CounterOpts{Name: "zeedfai_scorer_events_total", Help: "Eventos processados."})
	flagged = factory.NewCounter(prometheus.CounterOpts{Name: "zeedfai_scorer_flagged_total", Help: "Eventos marcados como suspeitos."})
	errors = factory.NewCounter(prometheus.CounterOpts{Name: "zeedfai_scorer_errors_total", Help: "Erros de processamento."})
	latency = factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "zeedfai_scorer_latency_seconds",
		Help:    "Latência de scoring por evento (SLO: p99.9 < 0.250s).",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	})

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(brokers, ",")...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
	)
	if err != nil {
		log.Fatalf("kafka client: %v", err)
	}
	defer cl.Close()

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	go func() { log.Fatal(http.ListenAndServe(":8080", nil)) }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("scorer: brokers=%s topic=%s group=%s", brokers, topic, group)

	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				log.Printf("fetch error: %v", e.Err)
				errors.Inc()
			}
			continue
		}
		faultRate, _ := strconv.ParseFloat(getenv("FAULT_RATE", "0"), 64)
		fetches.EachRecord(func(r *kgo.Record) {
			start := time.Now()
			if faultRate > 0 && rand.Float64() < faultRate {
				errors.Inc()
				return
			}
			var t Transaction
			if err := json.Unmarshal(r.Value, &t); err != nil {
				errors.Inc()
				return
			}
			if score(t) {
				flagged.Inc()
			}
			processed.Inc()
			latency.Observe(time.Since(start).Seconds())
		})
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
