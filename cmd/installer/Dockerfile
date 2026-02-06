FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
RUN apt-get update && apt-get install -y make

# installs semodule and all required libs to /dpkg
RUN cd /tmp && \
    apt-get download policycoreutils libsepol2 libsemanage2 libaudit1 libselinux1 libbz2-1.0 libcap-ng0 libpcre2-8-0 && \
    mkdir /dpkg && \
    for deb in *.deb; do dpkg --extract $deb /dpkg || exit 10; done

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags "-s -w" -a -o zeropod-installer cmd/installer/main.go
RUN GOARCH=$TARGETARCH make build

FROM gcr.io/distroless/base-debian13
WORKDIR /build
COPY --from=builder /workspace/zeropod-installer .
COPY --from=builder /workspace/containerd-shim-zeropod-v2 .
COPY --from=builder /dpkg /

ENTRYPOINT ["/build/zeropod-installer"]
