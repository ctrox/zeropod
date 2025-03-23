FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags "-s -w" -a -o zeropod-manager cmd/manager/main.go

# the manager runs criu lazy-pages, which needs libc so we can't use the static
# image here
FROM gcr.io/distroless/base-debian12
COPY --from=builder /workspace/zeropod-manager /
CMD ["/zeropod-manager"]
