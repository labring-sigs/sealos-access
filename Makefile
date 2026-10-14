TARGETARCH ?= amd64
GOOS ?= linux

.PHONY: all build
all: build

build: build-internal-user-controller build-broker build-sealos-access build-internal-user-admission build-internal-user-approval-adapter

.PHONY: build-internal-user-controller
build-internal-user-controller:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(TARGETARCH) go build -buildvcs=false -ldflags "-s -w" -trimpath -o bin/internal-user-controller-$(TARGETARCH) ./cmd/internal-user-controller

.PHONY: build-broker
build-broker:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(TARGETARCH) go build -buildvcs=false -ldflags "-s -w" -trimpath -o bin/broker-$(TARGETARCH) ./cmd/broker

.PHONY: build-sealos-access
build-sealos-access:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(TARGETARCH) go build -buildvcs=false -ldflags "-s -w" -trimpath -o bin/sealos-access-$(TARGETARCH) ./cmd/sealos-access

.PHONY: build-internal-user-admission
build-internal-user-admission:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(TARGETARCH) go build -buildvcs=false -ldflags "-s -w" -trimpath -o bin/internal-user-admission-$(TARGETARCH) ./cmd/internal-user-admission

.PHONY: build-internal-user-approval-adapter
build-internal-user-approval-adapter:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(TARGETARCH) go build -buildvcs=false -ldflags "-s -w" -trimpath -o bin/internal-user-approval-adapter-$(TARGETARCH) ./cmd/internal-user-approval-adapter

.PHONY: test
test:
	go test ./...
