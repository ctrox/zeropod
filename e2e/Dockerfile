FROM docker:28.0-cli

RUN apk add --update go make iptables
WORKDIR /app
ADD go.* /app
RUN go mod download
