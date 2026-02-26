REPOSITORY ?= deployment-tracker
TAG ?= latest
IMG := $(REPOSITORY):$(TAG)
CLUSTER = kind

.PHONY: build
build:
	go build -o deployment-tracker cmd/deployment-tracker/main.go

.PHONY: docker
docker:
	docker build --platform linux/arm64 -t ${IMG} .

.PHONY: kind-load-image
kind-load-image:
	kind load docker-image ${IMG} --name ${CLUSTER}

fmt:
	go fmt ./...

test:
	KUBEBUILDER_ASSETS=$$(setup-envtest use -p path) go test ./...

test-short:
	go test -short ./...
