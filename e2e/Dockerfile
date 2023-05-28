FROM golang:1.20

RUN apt update && apt install -y docker.io
WORKDIR /app
ADD go.* /app
RUN go mod download