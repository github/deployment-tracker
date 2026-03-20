REPOSITORY ?= deployment-tracker
TAG ?= latest
IMG := $(REPOSITORY):$(TAG)
CLUSTER = kind

.PHONY: run-local
run-local: cluster-delete cluster build docker kind-load-image echo deploy

.PHONY: build
build:
	go build -o deployment-tracker cmd/deployment-tracker/main.go

.PHONY: docker
docker:
	docker build --platform linux/arm64 -t ${IMG} .

.PHONY: kind-load-image
kind-load-image:
	kind load docker-image ${IMG} --name ${CLUSTER}

.PHONY: deploy
deploy:
	@echo "Deploying deployment-tracker to cluster..."
	kubectl apply -f deploy/manifest.yaml
	@echo "Deployment complete. Waiting for deployment to be ready..."
	kubectl rollout status deployment/deployment-tracker -n deployment-tracker --timeout=60s

fmt:
	go fmt ./...

test:
	go test ./...

integration-test:
	KUBEBUILDER_ASSETS=$$(setup-envtest use -p path) go test -tags integration_test ./...

test-short:
	go test -short ./...

.PHONY: cluster
cluster:
	@echo "Creating kind cluster: ${CLUSTER}"
	kind create cluster --name ${CLUSTER}
	@echo "Creating namespaces..."
	kubectl create namespace test1 --dry-run=client -o yaml | kubectl apply -f -
	kubectl create namespace test2 --dry-run=client -o yaml | kubectl apply -f -
	kubectl create namespace test3 --dry-run=client -o yaml | kubectl apply -f -
	@echo "Kind cluster '${CLUSTER}' created with namespaces: test1, test2, test3"

.PHONY: cluster-delete
cluster-delete:
	@echo "Deleting kind cluster: ${CLUSTER}"
	kind delete cluster --name ${CLUSTER}

# adds an echo server to the cluster that deployment tracker can point to to simulate 200 response
.PHONY: echo
echo:
	@echo "Deploying echo server to artifact-registry namespace..."
	kubectl create namespace artifact-registry --dry-run=client -o yaml | kubectl apply -f -
	kubectl run artifact-registry --image=ealen/echo-server:latest --port=80 -n artifact-registry --restart=Always --labels="app=artifact-registry"
	kubectl expose pod artifact-registry --port=9090 --target-port=80 --name=artifact-registry -n artifact-registry --type=ClusterIP
	@echo "Echo server deployed and reachable at http://artifact-registry.artifact-registry.svc.cluster.local:9090"

.PHONY: echo-delete
echo-delete:
	@echo "Deleting echo server from artifact-registry namespace..."
	kubectl delete service artifact-registry -n artifact-registry --ignore-not-found=true
	kubectl delete pod artifact-registry -n artifact-registry --ignore-not-found=true
	kubectl delete namespace artifact-registry --ignore-not-found=true
	@echo "Echo server deleted"
