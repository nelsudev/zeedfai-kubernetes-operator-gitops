SHELL := /bin/bash
export PATH := $(HOME)/.local/bin:$(HOME)/.local/go/bin:$(PATH)
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2

.PHONY: help tools build images generate demo-up demo-down deploy-sample run burst monitoring test test-integration

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

tools: ## Install go/kind/kubectl/helm/flux into ~/.local
	./scripts/bootstrap-tools.sh

build: ## Build the four components
	cd scorer && go build ./...
	cd loadgen && go build ./...
	cd operator && go build ./...
	cd platform-api && go build ./...

test: ## Unit tests (vet + go test)
	cd operator && go vet ./... && go test ./...
	cd scorer && go vet ./... && go test ./...
	cd loadgen && go vet ./... && go test ./...
	cd platform-api && go vet ./... && go test ./...

test-integration: ## Controller envtest suite (downloads a test kube-apiserver/etcd)
	cd operator && KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.32.x -p path)" go test ./controllers/... -v -count=1

generate: ## Regenerate deepcopy + CRDs + RBAC (and sync copies into gitops/)
	cd operator && $(CONTROLLER_GEN) object paths=./api/...
	cd operator && $(CONTROLLER_GEN) crd paths=./... output:crd:dir=./config/crd
	cd operator && $(CONTROLLER_GEN) rbac:roleName=zeedfai-operator paths=./... output:rbac:dir=./config/rbac
	cp operator/config/crd/platform.zeedfai.io_scoringpipelines.yaml gitops/infrastructure/crds/
	cp operator/config/rbac/role.yaml gitops/infrastructure/operator/clusterrole.yaml

images: ## Build the images and load them into kind
	docker build -t zeedfai/scorer:dev scorer/
	docker build -t zeedfai/loadgen:dev loadgen/
	kind load docker-image zeedfai/scorer:dev zeedfai/loadgen:dev --name zeedfai

demo-up: ## Bring up the local environment: kind + Strimzi/Kafka + monitoring + loadgen + CRD
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
	@echo "Environment ready. Now: 'make run' in one terminal and 'make deploy-sample' in another."

deploy-sample: ## Apply the example ScoringPipeline
	kubectl apply -f operator/config/samples/pipeline.yaml
	kubectl get scoringpipelines

run: ## Run the operator locally (out-of-cluster) against kind
	cd operator && go run .

burst: ## Fire a load burst: 2000 ev/s for 120s
	kubectl port-forward svc/loadgen 8081:8081 & PF=$$!; sleep 2; \
	curl -X POST 'http://localhost:8081/burst?rate=2000&seconds=120'; kill $$PF

monitoring: ## kube-prometheus-stack (ServiceMonitor/PrometheusRule CRDs required by the operator)
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
	helm upgrade --install monitoring prometheus-community/kube-prometheus-stack \
	  -n monitoring --create-namespace --set grafana.adminPassword=zeedfai \
	  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
	  --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
	  --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false
	@echo "Grafana: kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80 (admin/zeedfai)"

demo-down: ## Destroy the kind cluster
	kind delete cluster --name zeedfai
