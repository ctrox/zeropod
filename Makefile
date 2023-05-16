# build-kind can be used for fast local development. It just switches out
# the shim binary and restarts containerd to load the potentially updated
# config.
build-kind: build
	docker cp config.toml kind-control-plane:/etc/containerd/
	docker cp containerd-shim-zeropod-v2 kind-control-plane:/opt/zeropod/bin
	docker exec kind-control-plane systemctl restart containerd

install-kind: build-installer
	kind load docker-image ctrox/zeropod-installer:dev
	kubectl apply -f config/installer.yaml

build:
	GOOS=linux go build -ldflags "-s -w" -o containerd-shim-zeropod-v2 .

logs:
	docker exec -ti kind-control-plane journalctl -fu containerd

build-criu:
	docker build --platform=linux/amd64 -t docker.io/ctrox/criu:v3.18 -f criu/Dockerfile .

build-installer:
	docker build --platform=linux/amd64 -t docker.io/ctrox/zeropod-installer:dev -f Dockerfile.installer .