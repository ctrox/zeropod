FROM docker:29.6.1-cli

RUN apk add --update go make iptables
WORKDIR /app
ADD go.* /app
RUN go mod download
