SHELL := /bin/bash
export PATH := $(HOME)/.local/bin:$(HOME)/.local/go/bin:$(PATH)
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2

.PHONY: help tools build images generate demo-up demo-down deploy-sample run burst monitoring test

help: ## Lista os targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

tools: ## Instala go/kind/kubectl/helm/flux em ~/.local
	./scripts/bootstrap-tools.sh

build: ## Compila os quatro componentes
	cd scorer && go build ./...
	cd loadgen && go build ./...
	cd operator && go build ./...
	cd platform-api && go build ./...

test: ## Testes
	cd operator && go vet ./... && go test ./...
	cd scorer && go vet ./...
	cd loadgen && go vet ./...
	cd platform-api && go vet ./...

test-integration: ## Testes envtest do controller (descarrega kube-apiserver/etcd de teste)
	cd operator && KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.32.x -p path)" go test ./controllers/... -v -count=1

generate: ## Regenera deepcopy + CRDs + RBAC (e sincroniza cópias em gitops/)
	cd operator && $(CONTROLLER_GEN) object paths=./api/...
	cd operator && $(CONTROLLER_GEN) crd paths=./... output:crd:dir=./config/crd
	cd operator && $(CONTROLLER_GEN) rbac:roleName=zeedfai-operator paths=./... output:rbac:dir=./config/rbac
	cp operator/config/crd/platform.zeedfai.io_scoringpipelines.yaml gitops/infrastructure/crds/
	cp operator/config/rbac/role.yaml gitops/infrastructure/operator/clusterrole.yaml

images: ## Build das imagens e load para o kind
	docker build -t zeedfai/scorer:dev scorer/
	docker build -t zeedfai/loadgen:dev loadgen/
	kind load docker-image zeedfai/scorer:dev zeedfai/loadgen:dev --name zeedfai

demo-up: ## Sobe o ambiente local: kind + Strimzi/Kafka + monitoring + loadgen + CRD
	kind get clusters | grep -q '^zeedfai$$' || kind create cluster --config hack/kind-config.yaml
	helm repo add strimzi https://strimzi.io/charts/ --force-update
	helm upgrade --install strimzi strimzi/strimzi-kafka-operator -n kafka --create-namespace --wait
	kubectl apply -f hack/kafka.yaml
	kubectl -n kafka wait kafka/zeedfai --for=condition=Ready --timeout=300s
	$(MAKE) monitoring
	$(MAKE) images
	kubectl apply -f hack/loadgen.yaml
	kubectl apply -f operator/config/crd/
	@echo
	@echo "Ambiente pronto. Agora: 'make run' num terminal e 'make deploy-sample' noutro."

deploy-sample: ## Aplica o ScoringPipeline de exemplo
	kubectl apply -f operator/config/samples/pipeline.yaml
	kubectl get scoringpipelines

run: ## Corre o operator localmente (fora do cluster) contra o kind
	cd operator && go run .

burst: ## Dispara um burst de carga: 2000 ev/s durante 120s
	kubectl port-forward svc/loadgen 8081:8081 & PF=$$!; sleep 2; \
	curl -X POST 'http://localhost:8081/burst?rate=2000&seconds=120'; kill $$PF

monitoring: ## kube-prometheus-stack (CRDs ServiceMonitor/PrometheusRule requeridos pelo operator)
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
	helm upgrade --install monitoring prometheus-community/kube-prometheus-stack \
	  -n monitoring --create-namespace --set grafana.adminPassword=zeedfai \
	  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
	  --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
	  --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false
	@echo "Grafana: kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80 (admin/zeedfai)"

demo-down: ## Destrói o cluster kind
	kind delete cluster --name zeedfai
