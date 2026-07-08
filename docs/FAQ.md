# zeedfai — FAQ & gotchas

Real questions and real failure modes, most found by actually running the
thing on a fresh machine. If something breaks, start here.

## General

**Q: Do I need a cloud account to try this?**
No. Everything — operator, Kafka, Prometheus, autoscaling of pods *and* nodes —
runs on one Linux box with Docker. See `docs/LOCAL-DEMO-GUIDE.md`. Cloud
(Hetzner, `terraform/hetzner/`) is only the optional Phase 7.

**Q: What is this, in one sentence?**
A Kubernetes operator (Go) that runs fraud-scoring stream pipelines with
consumer-lag autoscaling, SLO self-healing, canary rollback, and GitOps
delivery — a platform-engineering portfolio piece, not a real fraud product.

**Q: Why is the Go module path `github.com/bastian/...` but the repo `nelsudev/...`?**
The module path is just an internal import prefix; it doesn't have to match the
remote. It's consistent across all four modules, so builds are fine. Renaming it
is cosmetic and deliberately not done to avoid a churny diff.

## Local setup

**Q: `make demo-up` fails with `no matches for kind Kafka`.**
The Strimzi CRDs weren't registered yet when the Kafka CR was applied. `make
demo-up` is idempotent — just run it again. (In the GitOps path this can't
happen: `infra-kafka-cluster` has a `dependsOn` on `infra-strimzi`.)

**Q: `make deploy-sample` pods are `ImagePullBackOff`.**
The dev-loop sample (`operator/config/samples/pipeline.yaml`) uses the
locally-built `zeedfai/scorer:dev` image, which `make images` builds and
`kind load`s. If you skipped `make demo-up` (which calls `make images`) or
built on a different kind cluster name, the image isn't in the node. Re-run
`make demo-up`. Do **not** confuse this with the GitOps sample
(`gitops/infrastructure/demo/pipeline.yaml`), which uses private GHCR images and
needs the `ghcr-pull` secret.

**Q: The operator won't start locally — `unable to find leader election namespace`.**
You're running it out-of-cluster (`make run`) with leader election on. It's off
by default out-of-cluster; only the in-cluster Deployment sets
`ENABLE_LEADER_ELECTION=true`. If you hit this you've set that env var yourself —
unset it.

**Q: Ports 8080/8082/8083 "address already in use" when running the operator.**
Something else on your box holds them (the operator uses 8082 health / 8083
metrics precisely to avoid the common 8080). Find it with `ss -ltnp | grep 808`
and kill the stray process — often a previous `go run` that didn't exit.

**Q: Pods go `CrashLoopBackOff` with `too many open files` after restarting Docker/kind.**
Host inotify limits exhausted (kind + k3d + other containers). Raise them:
`sudo sysctl -w fs.inotify.max_user_instances=512 fs.inotify.max_user_watches=1048576`
(persist in `/etc/sysctl.d/`). This bit us after the k3d node-scaling demo.

## Observability

**Q: The GUI charts (or Grafana) are empty; alerts never fire.**
The single most common trap. kube-prometheus-stack, by default, only scrapes
`ServiceMonitor`s labelled `release=monitoring` — so the operator's own
`ServiceMonitor`s are ignored and no `zeedfai_*` series exist. Both delivery
paths now disable that filter (`serviceMonitorSelectorNilUsesHelmValues=false`):
the GitOps `HelmRelease` and `make monitoring` both set it. If you installed
kube-prometheus-stack some other way, set those flags yourself. Verify with
`kubectl get servicemonitor -A` and a Prometheus query for
`zeedfai_scorer_events_total`.

**Q: Canary never rolls back even with a broken image.**
Same root cause as above: the canary analysis reads the error rate from
Prometheus and **fails open** — "no data" is treated as "healthy", not "broken",
so a canary can sail through if Prometheus isn't scraping it. Confirm metrics
flow first. This is a deliberate trade-off (an observability outage shouldn't
trigger spurious rollbacks); a production version would add a "no data for N
minutes → roll back" guard.

**Q: Prometheus just started and charts are blank.**
Give it ~1 minute for the first scrape cycle; ranges are 30 min so they fill in
gradually.

## Autoscaling

**Q: The pipeline doesn't scale even under load.**
The autoscaler reads Kafka consumer lag directly (not CPU). Check: (1) the
operator can reach Kafka — logs show `consumer lag unavailable` if not;
(2) `targetLagPerReplica` isn't set absurdly high; (3) the 30 s `cooldownSeconds`
hasn't just fired. The `LAG`/`DESIRED` printcolumns on `kubectl get
scoringpipeline` show the live decision.

**Q: Node autoscaler (k3d) does nothing when I scale up.**
Pods must be genuinely `Unschedulable` (insufficient cpu/memory), not merely
`Pending` on an image pull. `kubectl describe pod` should show
`FailedScheduling ... Insufficient cpu`. The demo forces this with 2-CPU
requests; adjust to your node size.

**Q: After deleting a k3d node the cluster shows it as `NotReady` forever.**
`k3d node delete` removes the container but can leave the Kubernetes Node object
orphaned. The autoscaler script now also `kubectl delete node`s it; if you
deleted a node by hand, do the same.

## GitOps / images

**Q: `flux bootstrap` fails on `Kubernetes version ... does not match >=1.33`.**
The Flux 2.9 CLI wants k8s ≥1.33, but kind ships 1.32. Use an older Flux CLI for
bootstrap (2.3.x works against 1.28+): download it once and run `flux bootstrap`
with it. The in-cluster controllers are unaffected.

**Q: Images are private — how do the deployments pull them?**
GHCR packages default to private and the `gh` OAuth token can't flip visibility
via API (returns 404). So the in-cluster Deployments use
`imagePullSecrets: [ghcr-pull]`, a `docker-registry` secret you create per
`gitops/infrastructure/operator/README.md` (not committed — it holds a token).
The local dev loop sidesteps this entirely by using `kind load` + a public-style
local tag.

**Q: Why are there two copies of loadgen/pipeline manifests (`hack/` vs `gitops/`)?**
Different jobs: `hack/*` is the fast dev loop (`kind load` local `:dev` images,
no registry); `gitops/infrastructure/*` is what Flux applies (versioned GHCR
images + pull secret). They're kept in sync by hand; see ARCHITECTURE.md §8.

## Cloud (Phase 7, not yet applied)

**Q: Will `terraform apply` on Hetzner just work?**
It's `terraform validate`-clean, but two things are environment-specific and may
need tweaking on first real apply: the k3s `--flannel-iface`/`--node-ip` values
(Hetzner private-network interface name) and the server type/region. Treat the
first apply as a smoke test. And remember: **`terraform destroy` when done** —
the nightly teardown Action is a backstop, not a substitute.

**Q: Why Hetzner and not the Contabo scripts for node scaling?**
Contabo bills monthly with no downgrade, so a 2-minute burst node costs a full
month — fine as fixed infra, wrong for elastic scaling. Hetzner bills hourly and
has an official cluster-autoscaler provider. The Contabo scripts remain as a
cheap-fixed-infra / API-automation demo.
