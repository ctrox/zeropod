install-kind: build
	podman cp containerd-shim-zeropod-v2 kind-control-plane:/opt/zeropod/bin

build:
	GOOS=linux GOARCH=arm64 go build -o containerd-shim-zeropod-v2 .