ARG CRIU_IMAGE=ghcr.io/ctrox/zeropod-criu:v4.2

FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -ldflags "-s -w" -a -o zeropod-manager ./cmd/manager/

FROM ${CRIU_IMAGE} AS criu
# the manager runs criu lazy-pages, which needs libc so we can't use the static
# image here
FROM gcr.io/distroless/base-debian12
COPY --from=builder /workspace/zeropod-manager /
COPY --from=criu /bin /bin
COPY --from=criu /lib /lib
CMD ["/zeropod-manager"]
