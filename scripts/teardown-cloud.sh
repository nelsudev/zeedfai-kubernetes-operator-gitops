#!/usr/bin/env bash
# Destrói qualquer recurso cloud de demo do zeedfai (Contabo + Hetzner).
# Idempotente e seguro por omissão: se as credenciais de um provider não
# estiverem definidas, esse provider é simplesmente ignorado (não falha).
#
# Usado pela GitHub Action .github/workflows/teardown-cloud-demo.yml como
# rede de segurança contra recursos esquecidos a gastar dinheiro, e pode ser
# corrido localmente da mesma forma.
set -uo pipefail

echo "=== zeedfai: teardown de recursos cloud de demo ==="

# --- Contabo -----------------------------------------------------------
if [[ -n "${CNTB_CLIENT_ID:-}" && -n "${CNTB_CLIENT_SECRET:-}" && -n "${CNTB_API_USER:-}" && -n "${CNTB_API_PASS:-}" ]]; then
  echo "--- Contabo: a listar instâncias ---"
  cd "$(dirname "$0")/contabo"
  TOKEN=$(./auth.sh)
  mapfile -t IDS < <(curl -fsS 'https://api.contabo.com/v1/compute/instances' \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "x-request-id: $(cat /proc/sys/kernel/random/uuid)" \
    | python3 -c '
import sys, json
for i in json.load(sys.stdin).get("data", []):
    if str(i.get("displayName","")).startswith("zeedfai"):
        print(i["instanceId"])
')
  if [[ ${#IDS[@]} -eq 0 ]]; then
    echo "Contabo: nenhuma instância 'zeedfai*' encontrada."
  else
    for id in "${IDS[@]}"; do
      echo "Contabo: a cancelar instância $id"
      ./delete-instance.sh "$id" || echo "AVISO: falha ao cancelar $id"
    done
  fi
  cd - >/dev/null
else
  echo "Contabo: credenciais não definidas (CNTB_*), a saltar."
fi

# --- Hetzner Cloud -------------------------------------------------------
if [[ -n "${HCLOUD_TOKEN:-}" ]]; then
  echo "--- Hetzner: a listar servers com label zeedfai=true ---"
  SERVERS=$(curl -fsS -H "Authorization: Bearer ${HCLOUD_TOKEN}" \
    "https://api.hetzner.cloud/v1/servers?label_selector=zeedfai%3Dtrue" \
    | python3 -c 'import sys,json;[print(s["id"]) for s in json.load(sys.stdin).get("servers",[])]')
  if [[ -z "$SERVERS" ]]; then
    echo "Hetzner: nenhum server com label zeedfai=true encontrado."
  else
    for id in $SERVERS; do
      echo "Hetzner: a apagar server $id"
      curl -fsS -X DELETE -H "Authorization: Bearer ${HCLOUD_TOKEN}" \
        "https://api.hetzner.cloud/v1/servers/$id" || echo "AVISO: falha ao apagar $id"
    done
  fi
else
  echo "Hetzner: HCLOUD_TOKEN não definido, a saltar."
fi

echo "=== teardown concluído ==="
