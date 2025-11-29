FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
RUN apt-get update && apt-get install -y make

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags "-s -w" -a -o zeropod-installer cmd/installer/main.go
RUN GOARCH=$TARGETARCH make build

FROM gcr.io/distroless/static-debian12
WORKDIR /build
COPY --from=builder /workspace/zeropod-installer .
COPY --from=builder /workspace/containerd-shim-zeropod-v2 .

ENTRYPOINT ["/build/zeropod-installer"]
