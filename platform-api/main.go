// zeedfai platform-api: an operations/DX facade over the ScoringPipelines.
//
// Read-only over the cluster (lists pipelines, proxies Prometheus metrics)
// plus triggering bursts on the loadgen (a test tool, not configuration).
// Configuration writes do NOT go through here — the path is a commit to the
// GitOps repo (see docs/ARCHITECTURE.md); a POST /pipelines that opened a
// PR would be the natural extension, documented but out of scope for now.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed static
var static embed.FS

var (
	gvr = schema.GroupVersionResource{Group: "platform.zeedfai.io", Version: "v1alpha1", Resource: "scoringpipelines"}

	prometheusURL = getenv("PROMETHEUS_URL", "http://monitoring-kube-prometheus-prometheus.monitoring.svc:9090")
	loadgenURL    = getenv("LOADGEN_URL", "http://loadgen.default.svc:8081")
	httpClient    = &http.Client{Timeout: 5 * time.Second}
)

const (
	defaultBurstRate    = 2000
	defaultBurstSeconds = 120
	maxBurstRate        = 5000
	maxBurstSeconds     = 300
)

func main() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// out of cluster (dev): use the kubeconfig
		cfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			log.Fatalf("kubeconfig: %v", err)
		}
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/pipelines", func(w http.ResponseWriter, r *http.Request) {
		list, err := dyn.Resource(gvr).List(r.Context(), metav1.ListOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		type pipeline struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Image     string `json:"image"`
			Replicas  int64  `json:"replicas"`
			Desired   int64  `json:"desired"`
			Lag       int64  `json:"lag"`
			Available string `json:"available"`
			Canary    string `json:"canary"`
			SLOms     int64  `json:"sloMs"`
		}
		out := []pipeline{}
		for _, item := range list.Items {
			p := pipeline{Name: item.GetName(), Namespace: item.GetNamespace()}
			p.Image, _, _ = unstructured.NestedString(item.Object, "spec", "model", "image")
			p.SLOms, _, _ = unstructured.NestedInt64(item.Object, "spec", "slo", "latencyP999Ms")
			p.Replicas, _, _ = unstructured.NestedInt64(item.Object, "status", "replicas")
			p.Desired, _, _ = unstructured.NestedInt64(item.Object, "status", "desiredReplicas")
			p.Lag, _, _ = unstructured.NestedInt64(item.Object, "status", "consumerLag")
			conds, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
			for _, c := range conds {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				switch cm["type"] {
				case "Available":
					p.Available, _ = cm["status"].(string)
				case "CanaryHealthy":
					p.Canary, _ = cm["reason"].(string)
				}
			}
			out = append(out, p)
		}
		writeJSON(w, out)
	})

	// Proxy of range-queries to Prometheus for the GUI's charts.
	// Only pre-defined queries — never arbitrary PromQL from the browser.
	mux.HandleFunc("GET /api/pipelines/{name}/metrics", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		queries := map[string]string{
			"lag":      fmt.Sprintf(`zeedfai_operator_consumer_lag{pipeline=%q}`, name),
			"replicas": fmt.Sprintf(`zeedfai_operator_ready_replicas{pipeline=%q}`, name),
			"p999ms":   fmt.Sprintf(`1000 * histogram_quantile(0.999, sum(rate(zeedfai_scorer_latency_seconds_bucket{pipeline=%q}[2m])) by (le))`, name),
			"rate":     fmt.Sprintf(`sum(rate(zeedfai_scorer_events_total{pipeline=%q}[1m]))`, name),
		}
		end := time.Now().Unix()
		start := end - 30*60
		out := map[string]any{}
		for key, q := range queries {
			u := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=15",
				strings.TrimRight(prometheusURL, "/"), url.QueryEscape(q), start, end)
			resp, err := httpClient.Get(u)
			if err != nil {
				continue
			}
			var body struct {
				Data struct {
					Result []struct {
						Values [][2]any `json:"values"`
					} `json:"result"`
				} `json:"data"`
			}
			err = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if err != nil || len(body.Data.Result) == 0 {
				out[key] = [][2]any{}
				continue
			}
			out[key] = body.Data.Result[0].Values
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("POST /api/burst", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rate, err := boundedIntParam(q, "rate", defaultBurstRate, 1, maxBurstRate)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seconds, err := boundedIntParam(q, "seconds", defaultBurstSeconds, 1, maxBurstSeconds)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		u := fmt.Sprintf("%s/burst?rate=%d&seconds=%d", strings.TrimRight(loadgenURL, "/"), rate, seconds)
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, u, nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	// GUI (files embedded in the binary — no external dependencies)
	staticFS, err := fs.Sub(static, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServerFS(staticFS))

	addr := getenv("LISTEN_ADDR", ":8090")
	log.Printf("platform-api listening on %s (prometheus=%s loadgen=%s)", addr, prometheusURL, loadgenURL)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func boundedIntParam(values url.Values, name string, def, min, max int) (int, error) {
	raw := values.Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return v, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
