# zeedfai na Contabo (cloud barata via API)

Alternativa low-cost à fase cloud (GKE/EKS): um VPS Contabo com **k3s + Flux**,
provisionado 100% via API — cumpre o mesmo papel de "demo em cloud real" por
~5€/mês, e demonstra automação de infraestrutura por API.

> Alternativa oficial aos scripts: o CLI **`cntb`** da Contabo
> (https://contabo.com/en/contabo-cli/) cobre as mesmas operações
> (`cntb create instance --userData "$(cat cloud-init.yaml)"`). Os scripts aqui
> usam a REST API diretamente (https://api.contabo.com) para mostrar a mecânica
> OAuth2 + endpoints sem dependências.

## Credenciais

No painel Contabo → API: cria `CLIENT_ID`, `CLIENT_SECRET`, e usa o teu user/pass da API.

```bash
export CNTB_CLIENT_ID=...
export CNTB_CLIENT_SECRET=...
export CNTB_API_USER=...
export CNTB_API_PASS=...
```

## Teardown automático (rede de segurança de custo)

`.github/workflows/teardown-cloud-demo.yml` corre `scripts/teardown-cloud.sh`
todas as noites (03:00 UTC) e também manualmente (aba Actions → "Run
workflow"). Destrói qualquer instância Contabo com `displayName` a começar
por `zeedfai` e qualquer server Hetzner com label `zeedfai=true`. Configura os
secrets do repo (Settings → Secrets and variables → Actions):
`CNTB_CLIENT_ID`, `CNTB_CLIENT_SECRET`, `CNTB_API_USER`, `CNTB_API_PASS`,
`HCLOUD_TOKEN`. Se não configurares nenhum, o workflow corre e não faz nada
(cada provider é ignorado sem credenciais).

## Uso

```bash
./create-vps.sh            # cria VPS (VPS 10, eu-west) com cloud-init k3s+flux
./list-instances.sh        # lista instâncias e IPs
# quando terminar a demo:
./delete-instance.sh <id>  # destrói (não esquecer!)
```

O `cloud-init.yaml` instala k3s, kubectl e faz `flux install`. Depois de teres o
IP: `ssh root@IP`, copia o kubeconfig (`/etc/rancher/k3s/k3s.yaml`) e faz
`flux bootstrap github ...` apontando ao teu repo GitOps.

> Nota: para a candidatura, a Contabo prova automação por API e gestão de
> infraestrutura; a checkbox "AWS ou GCP" da vaga cumpre-se melhor com a fase
> GKE/EKS + Terraform descrita em docs/. As duas não se excluem.
