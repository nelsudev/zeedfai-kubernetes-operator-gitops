# zeedfai on Hetzner Cloud (cloud phase)

An hourly-billed k3s cluster to demonstrate real **machine scaling** — which
Contabo (monthly billing) can't do with any economic honesty.

## Prerequisites

- Hetzner Cloud account + API token (project → Security → API tokens, Read & Write)
- `terraform` ≥ 1.6
- an SSH public key for root access to the nodes
- a server type available in your Hetzner project/location. The default is
  `ccx13` in `fsn1` because the 2026-07-09 validation run found `cx22`
  missing and the cheaper `cpx*` x86 SKUs sold out for new orders in this
  project. Check with `hcloud server-type list` and override
  `TF_VAR_server_type` / `TF_VAR_location` if needed.

## Bring up

```bash
cd terraform/hetzner
export TF_VAR_hcloud_token=<token>
export TF_VAR_ssh_public_key="$(cat ~/.ssh/id_ed25519.pub)"
terraform init && terraform apply
# follow the "next_steps" output
```

Optional override:

```bash
export TF_VAR_server_type=cpx31
export TF_VAR_location=hel1
```

By default, the firewall allows SSH from anywhere and keeps the public
Kubernetes API port (`6443`) closed. For direct remote `kubectl` access, set a
trusted source range before applying:

```bash
export TF_VAR_kube_api_allowed_cidrs='["203.0.113.10/32"]'
```

If `TF_VAR_kube_api_allowed_cidrs` is empty, use SSH port-forwarding or run
`kubectl` from the control-plane host.

Cost depends on the selected server type. The default creates 2× `ccx13`
instances, which were available for the real x86 validation run and cost about
€0.17/h total in `fsn1` at the 2026-07-09 listed price. Destroy them as soon
as the run is finished.

## Node scaling (cluster-autoscaler)

The [official cluster-autoscaler has a Hetzner provider](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/hetzner):
install it with the token and a node-group spec (`HCLOUD_CLUSTER_CONFIG`),
and when pods go `Pending` for lack of capacity it creates new servers in
~1 min, deleting them once capacity is freed up. Combined with the
zeedfai-operator's pod autoscaler, this gives the full demo: burst → more
replicas → no capacity → more **nodes** → burst ends → fewer replicas →
fewer nodes.

## Tear down (important)

```bash
terraform destroy
```

Safety net: every server carries the `zeedfai=true` label, and the
`teardown-cloud-demo.yml` GitHub Action deletes any server with that label
every night at 03:00 UTC (repo secret `HCLOUD_TOKEN`).
