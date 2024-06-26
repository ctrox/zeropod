FROM --platform=$BUILDPLATFORM golang:1.22 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download
RUN apt-get update && apt-get install -y make

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags "-s -w" -a -o zeropod-installer cmd/installer/main.go
RUN GOARCH=$TARGETARCH make build

ARG TARGETPLATFORM
FROM --platform=$TARGETPLATFORM centos/systemd
WORKDIR /build
COPY --from=builder /workspace/zeropod-installer .
COPY --from=builder /workspace/containerd-shim-zeropod-v2 .

ENTRYPOINT ["/build/zeropod-installer"]
