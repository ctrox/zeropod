FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/freezer cmd/freezer

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO111MODULE=on go build -ldflags "-s -w" -a -o freezer cmd/freezer/main.go

FROM gcr.io/distroless/static-debian12
COPY --from=builder /workspace/freezer /
ENTRYPOINT ["/freezer"]
