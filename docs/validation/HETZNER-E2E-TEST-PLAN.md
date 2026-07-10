# Hetzner E2E validation plan

Branch: `cloud/hetzner-e2e-validation`

Purpose: prove whether the GitOps platform can be brought up on real Hetzner
Cloud instances, record every failed prerequisite, and update the repo so the
next run is reproducible.

## Scope

- Provision the Terraform k3s cluster in `terraform/hetzner`.
- Validate SSH, cloud-init, k3s readiness, kubeconfig export, and node join.
- Install the platform stack with the repo manifests or document the first
  blocking gap.
- Run operator, pipeline lifecycle, autoscaling, observability, and GUI checks.
- Tear down all hourly-billed resources at the end of the run.

## Prerequisites to validate

| Check | Evidence | Status |
|---|---|---|
| Hetzner CLI context can list resources | `hcloud server list` exits 0 | Passed |
| Terraform can initialize providers | `terraform -chdir=terraform/hetzner init -backend=false` | Passed |
| Terraform configuration is valid | `terraform -chdir=terraform/hetzner validate` | Passed |
| Local Go toolchain is reachable with documented PATH | `PATH="$HOME/.local/bin:$HOME/.local/go/bin:$PATH" make test` | Passed |
| SSH public key exists or can be generated | `~/.ssh/zeedfai_hetzner_e2e.pub` | Passed |
| Hetzner server type exists in the project | `terraform apply` / `hcloud server-type list` | Failed with `cx22` and unavailable `cpx*`; fixed default to validated `ccx13` |

## Test matrix

### 1. Infrastructure provisioning

Commands:

```bash
TOKEN=$(awk -F"'" '/^[[:space:]]*token[[:space:]]*=/ {print $2; exit}' ~/.config/hcloud/cli.toml)
export TF_VAR_hcloud_token="$TOKEN"
export TF_VAR_ssh_public_key="$(cat ~/.ssh/zeedfai_hetzner_e2e.pub)"
terraform -chdir=terraform/hetzner plan -out=tfplan
terraform -chdir=terraform/hetzner apply -auto-approve tfplan
```

Pass criteria:

- Control-plane and worker servers are created with `zeedfai=true` labels.
- Both servers are `running` in Hetzner.
- SSH to the control-plane works with the generated key.
- `/var/lib/cloud/instance/boot-finished` exists on each node.

Run status: passed after the Terraform/cloud-init fixes recorded in
`HETZNER-E2E-REPORT-2026-07-09.md`.

### 2. k3s bootstrap

Commands:

```bash
ssh -i ~/.ssh/zeedfai_hetzner_e2e root@<control-plane-ip> 'systemctl is-active k3s && kubectl get nodes -o wide'
scp -i ~/.ssh/zeedfai_hetzner_e2e root@<control-plane-ip>:/etc/rancher/k3s/k3s.yaml /tmp/zeedfai-hetzner-kubeconfig
sed -i "s/127.0.0.1/<control-plane-ip>/" /tmp/zeedfai-hetzner-kubeconfig
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl get nodes -o wide
```

Pass criteria:

- The control-plane node is `Ready`.
- The worker node is `Ready`.
- Remote kubeconfig works from the local machine.

Run status: passed after adding private-network netplan config, k3s private
node IP flags, and public-IP `--tls-san`.

### 3. Platform install

Preferred GitOps route:

```bash
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig flux bootstrap github \
  --owner=nelsudev \
  --repository=zeedfai-kubernetes-operator-gitops \
  --branch=cloud/hetzner-e2e-validation \
  --path=gitops/clusters/staging \
  --components-extra=image-reflector-controller,image-automation-controller \
  --personal
```

Without `--components-extra`, the `infra-image-automation` Kustomization
fails permanently (`no matches for kind "ImageUpdateAutomation"`) because
the `ImageUpdateAutomation` CRD is never installed — this is already
documented in `docs/FAQ.md` and `docs/LOCAL-DEMO-GUIDE.md` for the local
demo, but was missing from this cloud test plan.

Fallback route if GitHub auth or image registry prevents bootstrap:

```bash
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig helm repo add strimzi https://strimzi.io/charts/ --force-update
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl apply -k gitops/infrastructure/crds
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl apply -k gitops/infrastructure/demo
```

Pass criteria:

- Flux kustomizations become `Ready`, or the fallback route documents the first
  missing dependency.
- Strimzi, Kafka, monitoring, operator, platform API, loadgen, and demo
  pipeline all reach ready state.

Run status: passed after adding the missing `infra-operator` dependency on
`infra-monitoring`.

### 4. Functional checks

Commands:

```bash
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl get scoringpipelines -A
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl get deploy,pod,svc,servicemonitor,prometheusrule -A
```

Pass criteria:

- `card-payments-eu` reports `Available=True`.
- The scorer Deployment converges to `spec.minReplicas`.
- Generated ServiceMonitor, PrometheusRule, and PDB exist.

Run status: passed.

### 5. Burst/autoscaling check

Commands:

```bash
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl port-forward svc/loadgen 8081:8081
curl -X POST 'http://localhost:8081/burst?rate=2000&seconds=120'
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl get scoringpipeline card-payments-eu -w
```

Pass criteria:

- Consumer lag rises during the burst.
- Desired scorer replicas rise above the minimum.
- Replicas scale back down after lag drains and cooldown expires.

Run status: passed. Observed 2 → 7 → 2 replicas.

### 6. Observability and GUI check

Commands:

```bash
KUBECONFIG=/tmp/zeedfai-hetzner-kubeconfig kubectl -n zeedfai-system port-forward svc/platform-api 8090:8090
curl -fsS http://localhost:8090/
```

Pass criteria:

- Platform API responds.
- GUI loads without external JavaScript dependencies.
- Prometheus-backed charts return non-empty series after scrape warm-up.

Run status: API and GUI HTML passed. Chart series were not separately captured
in this run.

### 7. Teardown

Commands:

```bash
terraform -chdir=terraform/hetzner destroy
hcloud server list -o columns=name,labels | grep zeedfai || true
```

Pass criteria:

- Terraform destroys all managed resources.
- No server with label `zeedfai=true` remains.

## Run log

- `terraform apply` with original `server_type = "cx22"` failed:
  `server type cx22 not found`.
- Terraform was changed to make `server_type` and `location` configurable and
  default to the validated x86 combination `ccx13` / `fsn1`.
- Teardown completed successfully; no `zeedfai=true` servers remained.
