# zeedfai — arquitetura e guia do repositório

Este documento explica **o que cada peça faz e porquê**, para quem chega ao
repo pela primeira vez (incluindo o próprio autor, três meses depois). Está
organizado por camada: domínio (Go), API Kubernetes, GitOps (Flux),
CI/CD, e scripts auxiliares.

Estado: Fases 0–6 completas e verificadas ao vivo num cluster kind (o teste
da Fase 5 encontrou e corrigiu dois bugs de observabilidade — ver secção
"Observabilidade: as duas armadilhas"). Falta a Fase 7 (cloud/Hetzner). Ver
`README.md` para o checklist atualizado.

---

## 1. O problema que o projeto simula

Um serviço de *fraud-scoring* consome transações de um tópico Kafka e decide,
por evento, se é suspeito. O SLA de referência da indústria (documentado no
paper do Railgun da Feedzai) é **scoring com latência p99.9 < 250ms**. O
projeto não implementa deteção de fraude a sério — a "regra" no `scorer` é
propositadamente trivial — porque o que está a ser demonstrado é a
**plataforma que opera esse serviço**: como ele escala, se autocura, é
entregue e observado.

---

## 2. Componentes Go

### 2.1 `scorer/main.go`

O serviço de negócio. Um binário Go pequeno e sem dependências além do
cliente Kafka e do cliente Prometheus.

- **O que faz:** liga-se ao Kafka como consumidor de um `consumerGroup`,
  lê transações JSON do tópico `transactions`, aplica `score()` (regra
  dummy: montante alto ou montante médio + país de risco → suspeito),
  incrementa contadores/histograma Prometheus, expõe tudo em `/metrics`.
- **Variáveis de ambiente** (todas injetadas pelo operator, nunca à mão):
  - `KAFKA_BROKERS`, `KAFKA_TOPIC`, `KAFKA_GROUP` — ligação ao Kafka.
  - `PIPELINE_NAME` — usado como label Prometheus `pipeline`, para que as
    `PrometheusRule` geradas pelo operator consigam filtrar alertas por
    pipeline individual (múltiplos `ScoringPipeline` no mesmo cluster não
    se confundem nas métricas).
  - `ROLE` (`stable` ou `canary`, default `stable`) — label Prometheus
    `role`, usado pela análise de canary (secção 4) para comparar a taxa de
    erro do candidato contra o baseline.
  - `FAULT_RATE` (float, default `0`) — só existe para **testar o rollback
    automático de canary**: se > 0, essa fração dos eventos é
    deliberadamente contada como erro e descartada. Nunca se define em
    produção; serve para construir uma imagem "candidata má" a propósito
    (ver `scorer/Dockerfile`, `ARG FAULT_RATE`).
- **Métricas expostas** (todas com labels `pipeline` e `role`):
  - `zeedfai_scorer_events_total` — eventos processados.
  - `zeedfai_scorer_flagged_total` — eventos marcados como suspeitos.
  - `zeedfai_scorer_errors_total` — falhas de parsing/fault injetado.
  - `zeedfai_scorer_latency_seconds` — histograma da latência de scoring;
    é sobre este que o `PrometheusRule` calcula o p99.9 do SLO.
- **Porquê um registry "wrapped"** (`prometheus.WrapRegistererWith`) em vez
  de `promauto.NewCounter` direto: as métricas só podem ser criadas depois
  de saber `PIPELINE_NAME`/`ROLE` (lidos do ambiente em `main()`), por isso
  o registo acontece dentro de `main()`, não em `var (...)` a nível de
  pacote como seria o padrão mais simples.

### 2.2 `loadgen/main.go`

Gerador de tráfego sintético — não faz parte do "produto", é ferramenta de
teste, mas corre em cluster tal como o scorer para poder ser controlado
remotamente durante uma demo.

- **O que faz:** produz transações aleatórias (cartão, montante, país,
  timestamp) para o tópico Kafka a uma taxa configurável (`BASE_RATE`,
  eventos/segundo).
- **`POST /burst?rate=N&seconds=S`:** o mecanismo central das demos deste
  projeto. Sobe a taxa de produção para `N` ev/s durante `S` segundos e
  depois volta sozinho ao `BASE_RATE` (via `time.AfterFunc`). É isto que o
  botão "🔥 Burst" da GUI (Fase 6) vai chamar, e foi usado manualmente
  (via `kubectl port-forward` + `curl`) para provar o autoscaler em ação:
  ver `README.md`, secção GitOps, para os números reais observados
  (lag 6350 → 10 réplicas → drenagem → 2 réplicas).

### 2.3 `operator/` — o Kubernetes Operator

Construído com [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
(o mesmo framework do kubebuilder), sem scaffolding completo do kubebuilder
CLI — os ficheiros foram escritos à mão para manter o repo pequeno e legível.

#### `operator/api/v1alpha1/scoringpipeline_types.go`

Define o CRD `ScoringPipeline` (grupo `platform.zeedfai.io`). Cada campo:

- `spec.model.image` — imagem do scorer **estável** (a que recebe a maioria
  do tráfego).
- `spec.model.imagePullSecret` — nome de um `Secret` docker-registry no
  mesmo namespace, para imagens privadas (GHCR). Opcional; vazio = imagem
  pública, sem pull secret.
- `spec.kafka.{brokers,topic,consumerGroup}` — ligação Kafka; se
  `consumerGroup` ficar vazio, o controller gera `zeedfai-<nome-do-pipeline>`.
- `spec.slo.latencyP999Ms` (default 250) — o número que dispara o alerta
  `ZeedfaiSLOLatencyViolated` e a self-healing (força scale-out).
- `spec.scaling.{minReplicas,maxReplicas,targetLagPerReplica,cooldownSeconds}`
  — parâmetros do autoscaler (secção 3).
- `spec.canary.{enabled,image,stepPercent,errorRateThresholdPct,evaluationSeconds}`
  — parâmetros da análise de canary (secção 4).
- `status.{replicas,desiredReplicas,consumerLag,lastScaleTime,canaryStartedAt,conditions}`
  — tudo o que o controller escreve de volta; nunca editado por humanos.
  `desiredReplicas`/`consumerLag` aparecem como colunas no `kubectl get`
  (`+kubebuilder:printcolumn`) para inspeção rápida sem `-o yaml`.

`zz_generated.deepcopy.go` é gerado por `controller-gen` (`make generate`) —
nunca editar à mão, é sobrescrito a cada `make generate`.

#### `operator/controllers/scoringpipeline_controller.go`

O reconciler principal — chamado sempre que um `ScoringPipeline` muda, e
também periodicamente (`RequeueAfter: 15s`, ver mais abaixo) para reavaliar
o autoscaler mesmo sem mudanças ao spec. Fluxo, por ordem:

1. Calcula `minReplicas`/`maxReplicas` (com defaults defensivos: mínimo 1,
   máximo nunca menor que o mínimo).
2. Chama `consumerLag()` (secção 3) e decide `replicas` (autoscaler +
   self-healing por SLO).
3. `controllerutil.CreateOrUpdate` no `Deployment` do scorer — injeta as
   env vars (`KAFKA_*`, `PIPELINE_NAME`, `ROLE=stable`), a
   `ReadinessProbe` em `/healthz`, e o `ImagePullSecrets` se aplicável.
   `SetControllerReference` faz o Deployment ser *owned* pelo
   `ScoringPipeline` — apagar o CR apaga tudo em cascata; e o `Owns()` no
   `SetupWithManager` faz o controller reagir também a mudanças diretas no
   Deployment (ex.: alguém apaga-o à mão → reconcile repõe-no, é o
   "self-healing básico" testado no README).
4. Cria/atualiza o `Service` (porta `metrics`, para o `ServiceMonitor`).
5. `reconcileObservability()` (secção "Observabilidade").
6. `reconcileCanary()` (secção 4).
7. Lê o `Deployment` real para saber `ReadyReplicas`, define a condition
   `Available` (`True` só quando réplicas prontas ≥ decididas).
8. `Status().Update()` — só o subrecurso de status, nunca o spec (separação
   spec/status é a convenção Kubernetes: humanos/GitOps escrevem spec,
   controllers escrevem status).
9. Devolve `RequeueAfter: 15 * time.Second` — **não** `ctrl.Result{}` vazio.
   Isto é deliberado: sem isto, o controller só reconciliaria quando o
   `ScoringPipeline` ou o `Deployment` mudassem, e nunca reagiria sozinho a
   uma subida de consumer lag (que não é um evento Kubernetes, é um número
   dentro do Kafka). O requeue periódico é o que torna o autoscaler
   "vivo" mesmo em repouso.

`SetupWithManager` regista `Owns(&appsv1.Deployment{})` e
`Owns(&corev1.Service{})` — o controller-runtime usa isto para saber que
mudanças nesses recursos (feitas por qualquer ator, não só por ele) devem
disparar um novo reconcile do `ScoringPipeline` dono.

#### `operator/controllers/autoscale.go`

Toda a lógica de decisão de escala, isolada do reconciler principal para
poder ser lida (e um dia testada) independentemente.

- `consumerLag(ctx, brokers, group, topic)` — abre um cliente Kafka efémero
  (`kgo.NewClient` + `kadm.NewClient`, a API de admin do
  [franz-go](https://github.com/twmb/franz-go)), busca os offsets
  *committed* do grupo (`FetchOffsets`) e os offsets de fim de partição
  (`ListEndOffsets`), soma `end - committed` por partição. Fecha o cliente
  no fim (`defer cl.Close()`) — criar um cliente por reconcile tem
  overhead, mas é aceitável ao número de pipelines que este projeto tem em
  mente (dezenas, não milhares); documentado como trade-off consciente, não
  descoberto tarde.
- `desiredReplicasFromLag(lag, targetLagPerReplica, min, max)` — divisão
  inteira **arredondada para cima** (`(lag + target - 1) / target`), para
  que qualquer lag > 0 acima do último múltiplo justifique mais uma
  réplica, depois recorta ao intervalo `[min, max]`.
- `sloLatencyViolated(ctx, pipeline, maxMs)` — faz uma query HTTP direta ao
  Prometheus (`histogram_quantile(0.999, ...)`) em vez de usar um cliente
  Prometheus completo — decisão deliberada de manter zero dependências
  extra para uma única query simples. Devolve `false` em qualquer erro
  (Prometheus em baixo, sem dados, JSON inesperado) — **fail-open**: uma
  falha de observabilidade não deve por si só disparar scale-out
  desnecessário; o pior cenário é o autoscaler ficar cego temporariamente
  ao SLO, não que ele reaja a ruído.
- `cooldownElapsed(last *time.Time, cooldown)` — `true` se `last == nil`
  (nunca escalou) ou se já passou tempo suficiente. Usado tanto para
  scale-out como scale-down, para não oscilar em tráfego bursty (ver o
  teste ao vivo no README: 10→7→3→2 réplicas em passos, não de um salto).

**Onde isto se liga ao reconciler:** a decisão de `replicas` combina três
fontes, por esta ordem de precedência — (1) o último `desiredReplicas`
persistido em `status` (para não perder o estado entre reconciles), (2) o
cálculo por lag, (3) o forçar de +1 se o SLO estiver violado — e só se
aplica de facto se o `cooldown` já tiver passado.

#### `operator/controllers/canary.go`

A Fase 5. Ver secção 4 abaixo — desenho explicado em detalhe porque a
decisão de arquitetura (rollback automático, promoção manual) não é óbvia
e merece justificação própria.

#### `operator/controllers/observability.go`

Gera, por `ScoringPipeline`, três recursos que dependem do
[prometheus-operator](https://github.com/prometheus-operator/prometheus-operator)
estar instalado no cluster (por isso `make demo-up` instala sempre o
`kube-prometheus-stack` — deixou de ser opcional):

- `ServiceMonitor` — diz ao Prometheus para fazer scrape ao `Service` do
  scorer (porta `metrics`, intervalo 15s).
- `PrometheusRule` — dois alertas:
  - `ZeedfaiSLOLatencyViolated`: p99.9 > `spec.slo.latencyP999Ms` por 5min.
  - `ZeedfaiConsumerLagGrowing`: `deriv(lag) > 0` por 5min (lag a crescer
    de forma sustentada, não um pico momentâneo).
  Ambos os alertas têm anotação `runbook_url` a apontar para
  `runbooks/*.md` — a convenção de que todo o alerta tem um runbook
  correspondente é deliberada (é o que a vaga da Feedzai pede
  explicitamente: "develop playbooks, runbooks, alerting").
- `PodDisruptionBudget` — `minAvailable = replicas - 1`, para que
  operações de manutenção do cluster (drain de nodes, upgrades) nunca
  tirem todas as réplicas ao mesmo tempo.

#### `operator/controllers/metrics.go`

Três gauges Prometheus que o **próprio operator** expõe (não o scorer):
`zeedfai_operator_{consumer_lag,desired_replicas,ready_replicas}`, com
label `pipeline`. Existem porque o `consumerLag` calculado em
`autoscale.go` só vive em `status.consumerLag` (um valor pontual, sem
histórico) — expor como métrica Prometheus dá-lhe uma série temporal, que é
o que a GUI da Fase 6 vai desenhar num gráfico lag-vs-réplicas-vs-tempo.

Detalhe que custou um bug: têm de ser registadas no
`sigs.k8s.io/controller-runtime/pkg/metrics.Registry` (via
`promauto.With(...)`), porque o metrics server do manager em `:8083` serve
**esse** registry — o `prometheus.DefaultRegisterer` do client_golang não é
servido pelo controller-runtime, e métricas registadas lá simplesmente
nunca apareceriam no scrape.

#### `operator/controllers/util.go`

Uma função (`intstrFromInt`) para converter `int` em `intstr.IntOrString`
— tão pequena que só existe como ficheiro próprio para não poluir o
controller principal com um import só para isto.

#### `operator/main.go`

O `main()` do binário: monta o `Scheme` (regista os tipos do
`ScoringPipeline`, do `client-go` standard, e também `monitoringv1`/
`policyv1` — sem isto o client do controller-runtime não sabe serializar
`ServiceMonitor`/`PodDisruptionBudget`), configura o manager
(`HealthProbeBindAddress :8082`, `Metrics :8083` — portas nãostandard
porque em dev local, fora do cluster, a `:8080` por vezes já está ocupada
por outro processo do utilizador), liga o `ScoringPipelineReconciler`, e
arranca.

- `ENABLE_LEADER_ELECTION` (env, default `false`): fora do cluster
  (`make run`, dev loop) não faz sentido fazer leader election (só há uma
  instância, e pedir uma `Lease` a um cluster onde não se corre é inútil);
  dentro do cluster (`gitops/infrastructure/operator/deployment.yaml`)
  fica `true`, para o dia em que se aumentar `replicas` do operator para
  alta disponibilidade (só uma réplica reconcilia de cada vez, as outras
  ficam em standby).

---

## 3. Autoscaler — resumo do fluxo (para quem só quer a versão curta)

```
a cada reconcile (ou a cada 15s por requeue):
  lag = soma do lag de todas as partições do consumer group  (via Kafka admin)
  desired = ceil(lag / targetLagPerReplica), recortado a [min, max]
  se p99.9 > SLO: desired += 1 (self-healing, ainda recortado a max)
  se desired != réplicas_atuais E já passou o cooldown:
    aplica desired, regista o timestamp da decisão
  senão:
    mantém a última decisão (não flapa)
```

Verificado ao vivo (ver README): burst de 3000 ev/s → lag sobe a 6350 →
scale-out 2→10 → lag drena → scale-down em passos 10→7→3→2, nunca de
repente, por causa do cooldown de 30s.

---

## 4. Canary — desenho e porquê (Fase 5)

**O problema que isto resolve:** publicar uma nova versão do scorer sem
arriscar que uma imagem defeituosa processe 100% do tráfego de fraude antes
de alguém dar por isso.

**A decisão de arquitetura mais importante deste componente:** o rollback
é automático, a promoção não é. Explicação:

- Se o operator também escrevesse `spec.model.image` sozinho para promover
  um canary saudável, isso entraria em conflito direto com o GitOps: o
  Flux tem o repo Git como fonte de verdade e reverteria essa escrita na
  próxima reconciliação, criando um "flip-flop" entre o que o operator
  quer e o que o Git diz. Escrever de volta ao spec a partir do controller
  quebra a garantia central do GitOps (o cluster é sempre um reflexo do
  Git, nunca o contrário).
- Por isso, quando o canary sobrevive à janela de avaliação sem exceder o
  threshold de erro, o operator **não** promove sozinho — marca a
  condition `CanaryHealthy=True` com a mensagem "safe to promote via a Git
  commit", e é um humano (ou um pipeline de CI) que faz o commit a mudar
  `spec.model.image` para a imagem candidata. Auditável, reversível, sem
  surpresas.
- Já o rollback é o lado onde a automação vale mais e o risco de agir
  sozinho é menor: parar de mandar tráfego para uma imagem que está a
  falhar é uma ação segura e reversível (o stable nunca deixou de correr),
  e esperar por um humano custaria minutos ou horas de fraude mal
  detetada. Por isso é imediato e automático.

**Como o canary recebe tráfego sem service mesh:** o Deployment canary usa
o **mesmo `KAFKA_GROUP`** que o stable. O protocolo de consumer group do
Kafka reparte as partições do tópico entre todos os processos que se juntam
ao grupo — logo, ao juntar N réplicas canary a um grupo que já tinha M
réplicas stable, o Kafka automaticamente dá ao canary uma fração das
partições (e portanto do tráfego) proporcional ao seu peso no grupo. Não é
preciso Istio/Linkerd nem um proxy de tráfego: o particionamento do Kafka
*é* o mecanismo de split.

**`spec.canary` campo a campo:**
- `enabled` + `image`: só há canary ativo se `enabled=true` **e**
  `image` diferente de `spec.model.image` (evita um canary "da mesma
  imagem", que não testaria nada).
- `stepPercent` (default 20): réplicas do canary = `ceil(réplicas_stable *
  stepPercent / 100)`, mínimo 1.
- `errorRateThresholdPct` (default 5): acima disto, rollback imediato.
- `evaluationSeconds` (default 120): tempo mínimo a correr sem violar o
  threshold antes de a condition passar a `CanaryHealthy=True`.

**Guard anti-loop (importante):** depois de um rollback, o spec continua
com `canary.enabled=true` e a mesma imagem — sem proteção, o próximo
reconcile recriaria o canary mau, que voltaria a falhar, num ciclo
infinito de criar→falhar→rollback. Por isso o rollback grava a condition
`CanaryHealthy=False/RolledBack` com `observedGeneration` igual à
Generation atual do spec, e o controller recusa-se a recriar o canary
enquanto a Generation não mudar. Qualquer edição ao spec (imagem nova,
`enabled: false`) incrementa a Generation e limpa o guard naturalmente.

**Trade-off fail-open da análise:** tal como no SLO check do autoscaler,
se o Prometheus estiver indisponível a `canaryErrorRatePct` devolve
"sem dados" e o canary continua a correr sem ser avaliado — uma falha de
observabilidade não dispara rollbacks espúrios, mas também significa que
um canary mau pode sobreviver mais tempo se o Prometheus cair ao mesmo
tempo. Aceitável para esta demo; em produção juntar-se-ia um guard de
"sem dados durante X minutos → rollback por precaução".

**Query Prometheus usada para a taxa de erro do canary**
(`canaryErrorRatePct` em `canary.go`):

```promql
100 * sum(rate(zeedfai_scorer_errors_total{pipeline="X",role="canary"}[2m]))
    / clamp_min(sum(rate(zeedfai_scorer_events_total{pipeline="X",role="canary"}[2m])), 1)
```

O `clamp_min(..., 1)` evita divisão por zero quando o canary ainda não
processou nenhum evento (denominador zero) — nesse caso a taxa de erro
resultante seria calculada sobre `1` em vez de `0`, ou seja, mantém-se
artificialmente baixa em vez de indefinida, o que é o comportamento
correto: "sem dados ainda" não deve ser tratado como "está a falhar".

**Como testar isto localmente (não fizemos ainda ao vivo neste repo — é o
próximo passo pendente):**
1. Build de uma imagem "candidata má":
   `docker build --build-arg FAULT_RATE=0.5 -t ghcr.io/nelsudev/zeedfai-scorer:bad-canary scorer/`
2. `kubectl patch scoringpipeline card-payments-eu --type merge -p '{"spec":{"canary":{"enabled":true,"image":"ghcr.io/nelsudev/zeedfai-scorer:bad-canary"}}}'`
   — atenção: este pipeline é gerido pelo Flux (`infra-demo`), portanto o
   patch é revertido na próxima sync (~30 min). Para um teste rápido serve;
   o caminho "correto" é editar `gitops/infrastructure/demo/pipeline.yaml`
   e commitar, que é exatamente como se faria em produção.
3. Observar: `<pipeline>-scorer-canary` Deployment aparece, e em minutos o
   Event `CanaryRolledBack` e a condition `CanaryHealthy=False` aparecem, e
   o Deployment canary desaparece sozinho.

---

## 4b. platform-api + GUI (Fase 6) — `platform-api/`

Fachada de operações/DX no estilo "mini control plane interno". Decisões:

- **Read-only sobre o cluster, por desenho.** `GET /api/pipelines` lista os
  `ScoringPipeline` via dynamic client (RBAC só com get/list/watch);
  escritas de configuração continuam a ser exclusivas do Git — um
  `POST /pipelines` que abrisse um PR no repo GitOps é a extensão natural,
  documentada mas fora de escopo. A exceção pragmática é `POST /api/burst`,
  que fala com o loadgen: é ferramenta de teste, não configuração.
- **`GET /api/pipelines/{name}/metrics`** faz proxy de **range-queries
  pré-definidas** ao Prometheus (lag, réplicas prontas, p99.9 em ms,
  throughput; últimos 30 min, step 15s). Nunca aceita PromQL vindo do
  browser — o proxy existe justamente para não expor o Prometheus.
- **GUI embebida no binário** (`go:embed`, um único `index.html` sem
  dependências externas): tabela de pipelines com estado/canary, quatro
  gráficos SVG de **série única** (um eixo por gráfico — nunca dual-axis),
  linha de SLO a 250 ms no gráfico de latência, crosshair+tooltip no hover,
  dark mode via `prefers-color-scheme`, e o botão "🔥 Burst".
- As séries de lag/réplicas vêm das gauges `zeedfai_operator_*` (não do
  scorer) — por isso existe o Service+ServiceMonitor do próprio operator em
  `gitops/infrastructure/operator/metrics.yaml`.

Aceder: `kubectl -n zeedfai-system port-forward svc/platform-api 8090:8090`
→ http://localhost:8090.

## 4c. Observabilidade: as duas armadilhas que o teste ao vivo apanhou

Registadas aqui porque são o tipo de falha silenciosa que só aparece em
execução — build, vet e até a demo do autoscaler passavam sem elas:

1. **O kube-prometheus-stack não seleciona ServiceMonitors de terceiros por
   default** — só os que têm o label `release=monitoring`. Resultado: zero
   séries `zeedfai_*` no Prometheus, e como o SLO check e a análise de
   canary "falham aberto", tudo parecia verde — um canary com 50% de erros
   passou a avaliação. Fix: `*SelectorNilUsesHelmValues: false` nos values
   do HelmRelease. Moral: fail-open em observabilidade exige um teste que
   prove que os dados fluem.
2. **ServiceMonitor seleciona Services por label, não por spec.selector.**
   O controller criava o Service sem labels no metadata (só com o selector
   de pods) — match impossível. Fix: labels no próprio Service.

---

## 5. GitOps (Flux) — `gitops/`

### 5.1 Porque a estrutura está dividida assim

```
gitops/
├── clusters/staging/          # o que o "flux bootstrap" aponta
│   ├── flux-system/           # gerado pelo flux bootstrap, não editar à mão
│   ├── sources.yaml           # Kustomization -> infrastructure/sources
│   ├── infra-crds.yaml        # Kustomization -> infrastructure/crds
│   ├── infra-strimzi.yaml     # Kustomization -> infrastructure/strimzi
│   ├── infra-kafka-cluster.yaml
│   ├── infra-monitoring.yaml
│   ├── infra-operator.yaml
│   └── infra-demo.yaml
└── infrastructure/            # o conteúdo real (HelmReleases, CRDs, etc.)
    ├── sources/                (HelmRepository: strimzi, prometheus-community)
    ├── crds/                   (o CRD ScoringPipeline)
    ├── strimzi/                (HelmRelease do operador Strimzi)
    ├── kafka-cluster/          (os CRs Kafka/KafkaNodePool/KafkaTopic)
    ├── monitoring/             (HelmRelease kube-prometheus-stack)
    ├── operator/               (Deployment do zeedfai-operator + RBAC)
    └── demo/                   (loadgen + o ScoringPipeline de exemplo)
```

Cada `clusters/staging/infra-*.yaml` é um objeto `Kustomization` do Flux
(`kustomize.toolkit.fluxcd.io/v1`) — não confundir com um
`kustomization.yaml` do Kustomize puro (que também existe, um por pasta em
`infrastructure/`, para o Flux saber quais ficheiros aplicar). A separação
entre "o que aponta" (`clusters/`) e "o que é apontado"
(`infrastructure/`) é a convenção standard de repos Flux multi-cluster:
`clusters/staging` e um futuro `clusters/prod` podem reutilizar as mesmas
pastas em `infrastructure/` com overlays diferentes.

### 5.2 A cadeia de dependências (`dependsOn`)

```
infra-sources ─┬─> infra-crds ─────────> infra-operator ─┐
               │                                          ├─> infra-demo
               └─> infra-strimzi ──> infra-kafka-cluster ─┘
               └─> infra-monitoring
```

Porquê esta ordem, explicitamente:
- **`infra-crds` antes de `infra-operator`**: o Deployment do operator só
  faz sentido depois de o CRD `ScoringPipeline` existir (senão o operator
  arranca e falha a fazer `watch` num tipo que a API server não conhece).
- **`infra-strimzi` antes de `infra-kafka-cluster`**: os CRs `Kafka`/
  `KafkaNodePool`/`KafkaTopic` só são reconhecidos pela API server depois
  de o Helm chart do Strimzi instalar os seus CRDs. Sem este `dependsOn`,
  a primeira aplicação falharia com "no matches for kind Kafka" — só
  resolveria eventualmente pelas retries do kustomize-controller, mas de
  forma não determinística. Isto foi de facto observado durante o
  desenvolvimento (ver histórico de commits) antes de se separar
  `strimzi/` (o operador) de `kafka-cluster/` (os CRs) em duas
  Kustomizations distintas.
- **`infra-operator` E `infra-kafka-cluster` antes de `infra-demo`**: o
  `ScoringPipeline` de exemplo em `demo/` precisa do operator já a correr
  (para ser reconciliado) e do Kafka já a existir (para o scorer conseguir
  ligar-se).
- **`infra-monitoring` é independente** — só depende de `infra-sources`
  (o `HelmRepository` do prometheus-community), corre em paralelo com o
  ramo do Strimzi.

### 5.3 `gitops/infrastructure/operator/`

- `namespace.yaml` — `zeedfai-system`, separado do `default` (onde correm
  os scorers) para isolar RBAC e recursos administrativos dos workloads.
- `rbac.yaml` — `ServiceAccount` + `ClusterRoleBinding` para o
  `ClusterRole` gerado.
- `clusterrole.yaml` — **gerado automaticamente** por `make generate` a
  partir dos comentários `+kubebuilder:rbac:...` espalhados pelo código do
  controller (ver `scoringpipeline_controller.go`). Nunca editar à mão —
  editar os markers no Go e correr `make generate`, que já sincroniza a
  cópia aqui.
- `deployment.yaml` — o operator a correr in-cluster, imagem GHCR,
  `imagePullSecrets: [ghcr-pull]` (ver `README.md` do mesmo diretório para
  como criar esse secret — não é comitado, teria de conter um token).
- `README.md` — explica exatamente isto: porque as imagens ficaram
  privadas (a API de mudança de visibilidade do GHCR devolveu 404 com o
  token OAuth do `gh` CLI) e como recriar o secret de pull.

### 5.4 `gitops/infrastructure/demo/`

O `ScoringPipeline` de exemplo (`pipeline.yaml`) e o `loadgen` (imagem
GHCR). Existe como pasta própria, separada de `operator/`, porque
semanticamente é "carga de demonstração", não infraestrutura — um cluster
de produção teria os seus próprios `ScoringPipeline`s reais aqui, geridos
por outra equipa/repo, não pelo mesmo Kustomization que instala o Kafka.

---

## 6. CI/CD (`.github/workflows/`)

### 6.1 `ci.yml`

Corre em cada push a `main` e em cada PR:
- `build-test`: `go build`, `go vet`, `go test` para os três módulos Go
  (`operator`, `scorer`, `loadgen` — três `go.mod` independentes,
  deliberadamente não um workspace único, para poderem ser versionados e
  publicados de forma independente).
- `docker-build`: builda as três imagens (sem push) só para apanhar erros
  de Dockerfile antes de se tentar publicar a sério.

### 6.2 `teardown-cloud-demo.yml`

A rede de segurança de custo pedida explicitamente: corre manualmente
(botão "Run workflow" no GitHub) e também todas as noites às 03:00 UTC.
Chama `scripts/teardown-cloud.sh`, que:
- Lista instâncias Contabo cujo `displayName` comece por `zeedfai` e
  cancela-as.
- Lista servers Hetzner com a label `zeedfai=true` e apaga-os.
- Se os secrets de um provider (`CNTB_*` ou `HCLOUD_TOKEN`) não estiverem
  configurados no repo, esse provider é simplesmente ignorado — o
  workflow nunca falha por falta de credenciais, porque o objetivo é ser
  uma rede de segurança sempre presente, não mais um passo manual a
  esquecer.

Neste momento (Fase 7 ainda não feita) isto não apaga nada de facto, porque
ainda não há nenhuma VM real na Contabo/Hetzner — mas já fica pronto para
quando a Fase 7 provisionar recursos cloud reais, evitando o cenário de
"esqueci-me de desligar e a fatura veio grande".

---

## 7. Scripts (`scripts/`)

- `bootstrap-tools.sh` — instala Go, kind, kubectl, helm, flux em
  `~/.local`, sem precisar de sudo. Corrido uma vez por máquina de
  desenvolvimento (`make tools`).
- `teardown-cloud.sh` — ver secção 6.2; também corre localmente
  (`bash scripts/teardown-cloud.sh`) para quem quiser desligar tudo à mão.
- `contabo/` — `auth.sh` (OAuth2 password grant), `create-vps.sh`
  (cria um VPS com `cloud-init.yaml` que instala k3s+Flux),
  `list-instances.sh`, `delete-instance.sh`. Ver o `README.md` local para
  a discussão sobre porque a Contabo (billing mensal, sem downgrade) não é
  o provider certo para demonstrar *elasticidade de máquinas* — só serve
  como infraestrutura fixa barata ou demonstração de automação por API. A
  Hetzner (billing à hora, autoscaler oficial) é a escolha para a Fase 7.

---

## 8. `hack/` vs `gitops/infrastructure/`

Ficheiros parecidos existem duas vezes de propósito:
- `hack/kafka.yaml`, `hack/loadgen.yaml` — usados pelo fluxo de
  desenvolvimento rápido (`make demo-up`), que usa `kind load docker-image`
  para meter imagens locais no cluster **sem precisar de as publicar**.
  Aponta para tags `:dev` construídas localmente.
- `gitops/infrastructure/kafka-cluster/kafka.yaml`,
  `gitops/infrastructure/demo/loadgen.yaml` — a versão que o Flux realmente
  aplica a partir do Git, com imagens GHCR versionadas (`:0.1.0`, etc.) e
  `imagePullSecrets`.

São mantidos em sincronia manualmente (não há um único source of truth
entre os dois) porque servem propósitos diferentes: um é o ciclo de
"código → `make demo-up` → testar em segundos", o outro é o ciclo real de
GitOps "commit → Flux aplica sozinho". Se um dia isto incomodar, a correção
seria gerar `hack/*.yaml` a partir de `gitops/infrastructure/*` com
Kustomize patches, mas para a escala deste projeto a duplicação explícita é
mais fácil de ler do que essa indireção.
