# Operator — imagem privada no GHCR

As imagens `ghcr.io/nelsudev/zeedfai-{operator,scorer,loadgen}` ficam privadas
por omissão (GHCR não faz `container:write` de visibilidade via OAuth App
token do `gh`). O Deployment do operator e os Deployments `scorer` gerados
pelo controller usam `imagePullSecrets: [ghcr-pull]`.

O secret **não é comitado** (é um token). Cria-o em cada cluster:

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
```

Em produção isto seria gerido por SOPS/Sealed Secrets/External Secrets — fora
de escopo para esta demo pessoal.
