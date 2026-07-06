# Runbook: SLO de latência p99.9 violado

**Alerta:** `ZeedfaiSLOLatencyViolated` — p99.9 acima do limite (default 250 ms) por >5 min.

## Impacto
Scoring fora do SLA de referência da indústria (Feedzai Railgun: <250ms p99.9);
risco de decisões de fraude a atrasar o fluxo de pagamento.

## Diagnóstico
1. `kubectl get scoringpipeline <nome> -o yaml` — ver `status.conditions` e réplicas atuais vs. `spec.scaling.maxReplicas`.
2. Grafana: painel de latência p99.9 vs. throughput vs. consumer lag no mesmo eixo temporal.
3. Causas comuns:
   - Burst de tráfego mais rápido do que o autoscaler consegue reagir (ver `targetLagPerReplica` e cooldown).
   - Nó/pod com throttling de CPU — `kubectl top pods`.
   - Kafka broker sob pressão (só 1 broker na demo local — sem HA).

## Mitigação
- Curto prazo: `kubectl scale deploy <nome>-scorer --replicas=N` manual enquanto se investiga.
- Se recorrente: rever `spec.scaling` do CRD via PR no repo GitOps (min/max/targetLag).

## Pós-incidente
Registar em `docs/postmortems/`; se a causa for capacidade insuficiente, avaliar
subir `maxReplicas` ou adicionar nodes (ver Hetzner node-autoscaler, Fase 7).
