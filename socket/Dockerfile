FROM golang:1.23 as gomod

WORKDIR /app
ADD go.* /app
# for some reason, doing go mod download in the golang container results in a
# way smaller image, so we'll do it here and do the rest in a separate stage.
RUN go mod download

# we use fedora since it has a recent version of bpftool
FROM fedora:41
RUN dnf install -y llvm clang bpftool libbpf-devel golang

RUN mkdir /headers
RUN cp /usr/include/bpf/bpf_* /headers
COPY socket/vmlinux.h.gz /headers
RUN gunzip /headers/vmlinux.h.gz

COPY --from=gomod /go /tmp
ENV GOCACHE=/tmp
ENV GOMODCACHE=/tmp/pkg/mod

RUN mkdir -m 777 /app
WORKDIR /app

ENTRYPOINT ["go", "generate", "./..."]
