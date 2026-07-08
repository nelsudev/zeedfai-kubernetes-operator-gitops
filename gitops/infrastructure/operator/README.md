# Operator — private image on GHCR

The `ghcr.io/nelsudev/zeedfai-{operator,scorer,loadgen,platform-api}` images
are private by default (GHCR won't flip visibility via the `gh` CLI's OAuth
App token). The operator Deployment and the `scorer` Deployments generated
by the controller use `imagePullSecrets: [ghcr-pull]`.

The secret is **not committed** (it's a token). Create it in each cluster:

```bash
kubectl create namespace zeedfai-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=nelsudev \
  --docker-password="$(gh auth token)" \
  -n zeedfai-system --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=nelsudev \
  --docker-password="$(gh auth token)" \
  -n default --dry-run=client -o yaml | kubectl apply -f -

# Flux image automation scans the private scorer image from flux-system.
kubectl create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=nelsudev \
  --docker-password="$(gh auth token)" \
  -n flux-system --dry-run=client -o yaml | kubectl apply -f -
```

In production this would be managed via SOPS/Sealed Secrets/External
Secrets — out of scope for this personal demo.
