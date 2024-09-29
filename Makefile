REGISTRY := ghcr.io
NAMESPACE := ctrox
INSTALLER_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-installer:dev
MANAGER_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-manager:dev
TEST_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-test:dev
CRIU_VERSION := v3.19
CRIU_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-criu:$(CRIU_VERSION)
DOCKER_SOCK := /var/run/docker.sock
EBPF_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-ebpf:dev
# versioning
PKG=github.com/ctrox/zeropod
CONTAINERD_PKG=github.com/containerd/containerd
VERSION ?= $(shell git describe --match 'v[0-9]*' --dirty='.m' --always --tags)
REVISION=$(shell git rev-parse HEAD)$(shell if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi)
LDFLAGS=-s -w
SHIM_LDFLAGS=-X $(CONTAINERD_PKG)/version.Version=$(VERSION) -X $(CONTAINERD_PKG)/version.Revision=$(REVISION) -X $(CONTAINERD_PKG)/version.Package=$(PKG) $(LDFLAGS)
GOARCH ?= $(shell go env GOARCH)

# build-kind can be used for fast local development. It just builds and
# switches out the shim binary. Running pods have to be recreated to make use
# of the new shim.
build-kind: build
	docker cp containerd-shim-zeropod-v2 kind-control-plane:/opt/zeropod/bin/

install-kind: build-installer build-manager
	kind load docker-image $(INSTALLER_IMAGE)
	kind load docker-image $(MANAGER_IMAGE)
	kubectl apply -k config/kind

build:
	CGO_ENABLED=0 GOARCH=$(GOARCH) GOOS=linux go build -ldflags '${SHIM_LDFLAGS}' -o containerd-shim-zeropod-v2 cmd/shim/main.go

logs:
	docker exec -ti kind-control-plane journalctl -fu containerd

build-criu:
	docker buildx build --push --platform linux/arm64,linux/amd64 --build-arg CRIU_VERSION=$(CRIU_VERSION) -t $(CRIU_IMAGE) -f criu/Dockerfile .

build-installer:
	docker build --load -t $(INSTALLER_IMAGE) -f cmd/installer/Dockerfile .

build-manager:
	docker build --load -t $(MANAGER_IMAGE) -f cmd/manager/Dockerfile .

build-test:
	docker build --load -t $(TEST_IMAGE) -f e2e/Dockerfile .

build-ebpf:
	docker build --load -t $(EBPF_IMAGE) -f socket/Dockerfile .

test-e2e:
	go test -v ./e2e/

bench:
	go test -bench=. -benchtime=10x -v -run=Bench ./e2e/

test:
	go test -v -short ./...

# docker-e2e runs the e2e test in a docker container. However, as running the
# e2e test requires a docker socket (for kind), this mounts the docker socket
# of the host into the container. For now this is the only way to run the e2e
# tests on Mac OS with apple silicon as the shim requires GOOS=linux.
docker-test-e2e: build-test
	docker run --rm --privileged --network=host --rm -v $(DOCKER_SOCK):$(DOCKER_SOCK) -v $(PWD):/app $(TEST_IMAGE) make test-e2e

docker-bench: build-test
	docker run --rm --privileged --network=host --rm -v $(DOCKER_SOCK):$(DOCKER_SOCK) -v $(PWD):/app $(TEST_IMAGE) make bench

# has to have SYS_ADMIN because the test tries to set netns and mount bpffs
# we use --pid=host to make the ebpf tracker work without a pid resolver
docker-test:
	docker run --rm --cap-add=SYS_ADMIN --cap-add=NET_ADMIN --pid=host -v $(PWD):/app $(TEST_IMAGE) make test

CLANG ?= clang
CFLAGS := -O2 -g -Wall -Werror

# $BPF_CLANG is used in go:generate invocations.
# generate runs go generate in a docker container which has all the required
# dependencies installed.
generate: export BPF_CLANG := $(CLANG)
generate: export BPF_CFLAGS := $(CFLAGS)
generate: ttrpc
	docker run --rm -v $(PWD):/app:Z --user $(shell id -u):$(shell id -g) --env=BPF_CLANG="$(CLANG)" --env=BPF_CFLAGS="$(CFLAGS)" $(EBPF_IMAGE)

ttrpc:
	go mod download
	cd api/shim/v1; protoc --go_out=. --go_opt=paths=source_relative \
	--ttrpc_out=. --plugin=protoc-gen-ttrpc=`which protoc-gen-go-ttrpc` \
	--ttrpc_opt=paths=source_relative *.proto -I. \
	-I $(shell go env GOMODCACHE)/github.com/prometheus/client_model@v0.6.1

# to improve reproducibility of the bpf builds, we dump the vmlinux.h and
# store it compressed in git instead of dumping it during the build.
update-vmlinux:
	docker run --rm -v $(PWD):/app:Z --entrypoint /bin/sh --user $(shell id -u):$(shell id -g) $(EBPF_IMAGE) \
		-c "bpftool btf dump file /sys/kernel/btf/vmlinux format c" | gzip > socket/vmlinux.h.gz
