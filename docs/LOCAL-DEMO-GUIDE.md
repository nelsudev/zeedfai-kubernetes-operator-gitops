# zeedfai — Local Demo Guide

Everything in this guide runs on a single Linux machine with Docker — no cloud
account needed. It walks through the four demos this project is built around:

1. **Pipeline lifecycle & self-healing** — the operator reconciles a
   `ScoringPipeline` into a running workload and repairs manual damage.
2. **Pod autoscaling under burst** — consumer-lag-driven scale-out/in.
3. **Canary with automatic rollback** — a deliberately bad image gets
   detected and rolled back without human action.
4. **Node autoscaling (k3d)** — machines joining/leaving the cluster in
   response to `Pending` pods, simulating the Hetzner cluster-autoscaler.

Every step below was executed and verified on a real machine; timings quoted
are from those runs.

## Prerequisites

- Linux with Docker (8 GB+ RAM free recommended; the full stack — Kafka,
  Prometheus, Grafana, operator — is memory-hungry)
- `git`, `curl`, `make`

```bash
git clone https://github.com/nelsudev/zeedfai-kubernetes-operator-gitops && cd zeedfai-kubernetes-operator-gitops
make tools     # installs go, kind, kubectl, helm, flux into ~/.local (no sudo)
export PATH="$HOME/.local/bin:$HOME/.local/go/bin:$PATH"
```

## Demo 1 — Bring the platform up

```bash
make demo-up   # kind cluster + Strimzi/Kafka + kube-prometheus-stack + loadgen + CRD (~5–8 min)
```

Two ways to run the operator:

- **Dev loop (no registry needed):** `make run` in one terminal (runs the
  operator outside the cluster against kind), then `make deploy-sample`.
- **Full GitOps (what production looks like):** fork the repo, run
  `flux bootstrap github --owner=<you> --repository=zeedfai-kubernetes-operator-gitops --branch=main
  --path=gitops/clusters/staging --personal
  --components-extra=image-reflector-controller,image-automation-controller`,
  and create the `ghcr-pull` secret per
  `gitops/infrastructure/operator/README.md` in `zeedfai-system`, `default`,
  and `flux-system`. Flux then installs
  everything, including the operator in-cluster.

Verify:

```bash
kubectl get scoringpipelines
# NAME               REPLICAS   DESIRED   LAG   AVAILABLE
# card-payments-eu   2          2         300   True
```

**Self-healing check** — delete the workload, watch the operator restore it:

```bash
kubectl delete deploy card-payments-eu-scorer
kubectl get deploy -w        # recreated in seconds, back to 2/2
```

## Demo 2 — Pod autoscaling under burst

The operator polls Kafka consumer lag every 15 s and sizes the scorer
Deployment as `ceil(lag / targetLagPerReplica)`, clamped to
`[minReplicas, maxReplicas]`, with a 30 s cooldown against flapping. If the
p99.9 latency SLO (250 ms) is violated, it forces one extra replica.

```bash
make burst     # 2000 ev/s for 120 s against the loadgen
kubectl get scoringpipeline card-payments-eu -w
```

Observed on a real run (3000 ev/s burst): lag climbed to ~6 350 → replicas
went 2→10; as the backlog drained, cooldown-paced scale-in stepped
10→7→3→2. The `LAG` and `DESIRED` printcolumns tell the story live.

## Demo 3 — Canary with automatic rollback

The scorer image accepts a build-time fault rate, which lets you build a
deliberately broken candidate:

```bash
docker build --build-arg FAULT_RATE=0.5 -t <registry>/zeedfai-scorer:bad-canary scorer/
docker push <registry>/zeedfai-scorer:bad-canary
```

Enable the canary **via Git** (the pipeline is Flux-managed; a `kubectl
patch` would be reverted on the next sync) — edit
`gitops/infrastructure/demo/pipeline.yaml`:

```yaml
  canary:
    enabled: true
    image: <registry>/zeedfai-scorer:bad-canary
    stepPercent: 20            # canary replicas = ceil(20% of stable)
    errorRateThresholdPct: 5
    evaluationSeconds: 120
```

Commit, push, `flux reconcile kustomization infra-demo`. The canary shares
the stable consumers' Kafka group, so partition rebalancing gives it a
proportional slice of real traffic — no service mesh required. Watch:

```bash
kubectl get deploy -w                    # <name>-scorer-canary appears
kubectl get events -w | grep -i canary
```

Observed: with 50 % injected faults the measured error rate crossed the 5 %
threshold and the operator rolled back in **~80 s** — Event
`CanaryRolledBack`, canary Deployment deleted, and a generation-scoped guard
prevents recreating the same bad spec (change the spec to try again). A
healthy canary instead reaches `CanaryHealthy=True` after the evaluation
window; promotion is deliberately a Git commit, never automatic — the
operator must not fight Flux over `spec.model.image`.

## Demo 4 — Node autoscaling on localhost (k3d)

kind can't add nodes at runtime, but **k3d** (k3s-in-Docker) can — which
makes it possible to demonstrate *machine* scaling with Docker containers
playing the role of cloud VMs. `scripts/k3d-node-autoscaler.sh` implements
the same semantics as the real cluster-autoscaler: unschedulable pods →
create a node; drained-empty agent → remove it.

> Heads-up on resources: if the kind demo is running, pause it first —
> `docker stop zeedfai-control-plane zeedfai-worker` (fully reversible with
> `docker start` later).

```bash
# 1. Install k3d and create a minimal single-node cluster
curl -fsSLo ~/.local/bin/k3d https://github.com/k3d-io/k3d/releases/download/v5.8.3/k3d-linux-amd64
chmod +x ~/.local/bin/k3d
k3d cluster create zeedfai-nodes --servers 1 --agents 0 \
  --k3s-arg '--disable=traefik,servicelb,metrics-server@server:0' --no-lb --wait

# 2. Start the node autoscaler (max 3 extra nodes)
./scripts/k3d-node-autoscaler.sh zeedfai-nodes 3 &

# 3. Deploy a workload whose requests exceed one node's capacity
kubectl create deployment scorer-mock --image=rancher/mirrored-pause:3.6 --replicas=2
kubectl set resources deployment scorer-mock --requests=cpu=2,memory=512Mi

# 4. Burst: more replicas than the node can hold
kubectl scale deploy scorer-mock --replicas=6
kubectl get nodes -w
```

Observed on a real run:

```
06:02:44 node-autoscaler: SCALE-OUT: 3 pod(s) Unschedulable, agents=0 → creating node
06:02:48 node-autoscaler: node created (agents=1); cooldown 20s
          # 4 seconds from Pending to new Ready node; all 6 pods Running, 3+3 across nodes
06:03:30 node-autoscaler: SCALE-IN: node k3d-scaled-... has no workload → removing
          # after `kubectl scale --replicas=2`: idle node drained and deleted
```

Cleanup:

```bash
k3d cluster delete zeedfai-nodes
docker start zeedfai-control-plane zeedfai-worker   # resume the kind demo
```

On Hetzner (see `terraform/hetzner/`) the flow is identical, with the
official cluster-autoscaler Hetzner provider creating real hourly-billed
servers instead of Docker containers — that's the only difference.

## Demo 5 (bonus) — Operations GUI

With the GitOps deployment running:

```bash
kubectl -n zeedfai-system port-forward svc/platform-api 8090:8090
# open http://localhost:8090
```

Pipeline table (status, canary state), 30-minute charts for consumer lag,
ready replicas, p99.9 latency vs the 250 ms SLO line, and throughput — plus
a 🔥 Burst button that drives Demo 2 without touching a terminal.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `no matches for kind Kafka` on demo-up | Strimzi CRDs not ready yet — rerun `make demo-up` (idempotent) |
| Charts empty in the GUI | Prometheus needs ~1 min after startup for first scrapes; also check `kubectl get servicemonitor -A` |
| Canary never rolls back | Confirm Prometheus is scraping the scorer: the analysis fails open ("no data" ≠ "failing") — see docs/ARCHITECTURE.md § observability pitfalls |
| Node autoscaler does nothing | Pods must be `Unschedulable` (check `kubectl describe pod`), not just Pending on image pulls |
| VM out of memory | Run Demo 4 with the kind cluster stopped; the full kind stack needs ~4 GB |
