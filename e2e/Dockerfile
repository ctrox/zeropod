# FROM docker:29.1.2-cli
FROM golang:1.25

# RUN apk add --update go make iptables
WORKDIR /app
ADD go.* /app
RUN go mod download
