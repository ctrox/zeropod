REGISTRY := ghcr.io
NAMESPACE := ctrox
TAG := dev
INSTALLER_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-installer:$(TAG)
MANAGER_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-manager:$(TAG)
TEST_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-test:$(TAG)
# includes fix for https://github.com/checkpoint-restore/criu/issues/2532
CRIU_VERSION := 116991736c7641258a5b7f53f5079b90fc80b99e
CRIU_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-criu:$(CRIU_VERSION)
DOCKER_SOCK := /var/run/docker.sock
EBPF_IMAGE := $(REGISTRY)/$(NAMESPACE)/zeropod-ebpf:$(TAG)
# versioning
PKG=github.com/ctrox/zeropod
CONTAINERD_PKG=github.com/containerd/containerd/v2
VERSION ?= $(shell git describe --match 'v[0-9]*' --dirty='.m' --always --tags)
REVISION=$(shell git rev-parse HEAD)$(shell if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi)
SHIM_EXTLDFLAGS="-static" -s -w
SHIM_TAGS="no_grpc"
SHIM_LDFLAGS=-X $(CONTAINERD_PKG)/version.Version=$(VERSION) -X $(CONTAINERD_PKG)/version.Revision=$(REVISION) -X $(CONTAINERD_PKG)/version.Package=$(PKG) -extldflags ${SHIM_EXTLDFLAGS}
GOARCH ?= $(shell go env GOARCH)

# build-kind can be used for fast local development. It just builds and
# switches out the shim binary. Running pods have to be recreated to make use
# of the new shim.
build-kind: build
	docker cp containerd-shim-zeropod-v2 kind-worker:/opt/zeropod/bin/
	docker cp containerd-shim-zeropod-v2 kind-worker2:/opt/zeropod/bin/

install-kind: build-installer build-manager
	kind load docker-image $(INSTALLER_IMAGE)
	kind load docker-image $(MANAGER_IMAGE)
	kubectl apply --context kind-kind -k config/kind

install-manager: build-manager
	kind load docker-image $(MANAGER_IMAGE)
	kubectl --context kind-kind -n zeropod-system delete pods -l app.kubernetes.io/name=zeropod-node

build:
	CGO_ENABLED=0 GOARCH=$(GOARCH) GOOS=linux go build -ldflags '${SHIM_LDFLAGS}' -tags ${SHIM_TAGS} -o containerd-shim-zeropod-v2 cmd/shim/main.go

logs:
	docker exec kind-worker journalctl -fu containerd & docker exec kind-worker2 journalctl -fu containerd

build-criu:
	docker buildx build --push --platform linux/arm64,linux/amd64 --build-arg CRIU_VERSION=$(CRIU_VERSION) -t $(CRIU_IMAGE) -f criu/Dockerfile .

build-installer:
	docker build --load -t $(INSTALLER_IMAGE) -f cmd/installer/Dockerfile .

build-manager:
	docker build --build-arg CRIU_IMAGE=$(CRIU_IMAGE) --load -t $(MANAGER_IMAGE) -f cmd/manager/Dockerfile .

build-test:
	docker build --load -t $(TEST_IMAGE) -f e2e/Dockerfile .

build-ebpf:
	docker build --load -t $(EBPF_IMAGE) -f activator/Dockerfile .

push-dev: build-installer build-manager
	docker push $(INSTALLER_IMAGE)
	docker push $(MANAGER_IMAGE)

test-e2e:
	go test -timeout=30m -v ./e2e/ $(testargs)

bench:
	go test -bench=. -benchtime=10x -v -run=Bench ./e2e/

test:
	go test -v -short ./... $(testargs)

# docker-e2e runs the e2e test in a docker container. However, as running the
# e2e test requires a docker socket (for kind), this mounts the docker socket
# of the host into the container. For now this is the only way to run the e2e
# tests on Mac OS with apple silicon as the shim requires GOOS=linux.
docker-test-e2e: build-test
	docker run --rm --privileged --network=host --rm -v $(DOCKER_SOCK):$(DOCKER_SOCK) -v $(PWD):/app $(TEST_IMAGE) make test-e2e

docker-bench: build-test
	docker run --rm --privileged --network=host --rm -v $(DOCKER_SOCK):$(DOCKER_SOCK) -v $(PWD):/app $(TEST_IMAGE) make bench

# has to have SYS_ADMIN because the test tries to set netns and mount bpffs
docker-test: build-test
	docker run --rm --cap-add=SYS_ADMIN --cap-add=NET_ADMIN --pid=host --userns=host -v $(PWD):/app $(TEST_IMAGE) go test -v -short ./... $(testargs)

CLANG ?= clang
CFLAGS := -O2 -g -Wall -Werror

# $BPF_CLANG is used in go:generate invocations.
# generate runs go generate in a docker container which has all the required
# dependencies installed.
generate: export BPF_CLANG := $(CLANG)
generate: export BPF_CFLAGS := $(CFLAGS)
generate: ttrpc ebpf
	go generate ./api/...

ebpf: ebpf-built-or-build-ebpf
	docker run --rm -v $(PWD):/app:Z --user $(shell id -u):$(shell id -g) --userns=host --env=BPF_CLANG="$(CLANG)" --env=BPF_CFLAGS="$(CFLAGS)" $(EBPF_IMAGE)

ttrpc:
	go mod download
	cd api/shim/v1; protoc --go_out=. --go_opt=paths=source_relative \
	--ttrpc_out=. --plugin=protoc-gen-ttrpc=`which protoc-gen-go-ttrpc` \
	--ttrpc_opt=paths=source_relative *.proto -I. \
	-I $(shell go env GOMODCACHE)/github.com/prometheus/client_model@v0.6.1
	cd api/node/v1; protoc --go_out=. --go_opt=paths=source_relative \
	--ttrpc_out=. --plugin=protoc-gen-ttrpc=`which protoc-gen-go-ttrpc` \
	--ttrpc_opt=paths=source_relative *.proto -I.

# to improve reproducibility of the bpf builds, we dump the vmlinux.h and
# store it compressed in git instead of dumping it during the build.

update-vmlinux: ebpf-built-or-build-ebpf
	docker run --rm -v $(PWD):/app:Z --entrypoint /bin/sh --user $(shell id -u):$(shell id -g) $(EBPF_IMAGE) \
		-c "bpftool btf dump file /sys/kernel/btf/vmlinux format c" | gzip > activator/vmlinux.h.gz

ebpf-built-or-build-ebpf:
	docker image inspect $(EBPF_IMAGE) || $(MAKE) build-ebpf
