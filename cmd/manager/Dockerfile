ARG CRIU_IMAGE_NAME=ghcr.io/ctrox/zeropod-criu
ARG CRIU_VERSION=116991736c7641258a5b7f53f5079b90fc80b99e

FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags "-s -w" -a -o zeropod-manager cmd/manager/main.go

FROM ${CRIU_IMAGE_NAME}:${CRIU_VERSION} AS criu
# the manager runs criu lazy-pages, which needs libc so we can't use the static
# image here
FROM gcr.io/distroless/base-debian12
COPY --from=builder /workspace/zeropod-manager /
COPY --from=criu /bin /bin
COPY --from=criu /lib /lib
CMD ["/zeropod-manager"]
