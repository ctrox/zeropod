install-kind: build
	docker cp config.toml kind-control-plane:/etc/containerd/
	docker exec kind-control-plane systemctl restart containerd
	docker cp containerd-shim-zeropod-v2 kind-control-plane:/opt/zeropod/bin

build:
	GOOS=linux go build -o containerd-shim-zeropod-v2 .

logs:
	docker exec -ti kind-control-plane journalctl -fu containerd

install-criu-kind:
	docker exec kind-control-plane bash -c "which criu || (apt update && apt install -y software-properties-common && add-apt-repository ppa:criu/ppa && apt install -y criu)"
	docker exec kind-control-plane mkdir -p /etc/criu
	docker cp criu-default.conf kind-control-plane:/etc/criu/default.conf
