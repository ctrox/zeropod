FROM golang:1.20 as gomod

WORKDIR /app
ADD go.* /app
# for some reason, doing go mod download in the golang container results in a
# way smaller image, so we'll do it here and do the rest in a separate stage.
RUN go mod download

# we use fedora since it has a recent version of bpftool
FROM fedora:38
RUN dnf install -y llvm clang bpftool libbpf-devel golang

RUN mkdir /headers
RUN cp /usr/include/bpf/bpf_helper* /headers
RUN bpftool btf dump file /sys/kernel/btf/vmlinux format c > /headers/vmlinux.h

COPY --from=gomod /go /root/go
WORKDIR /app

ENTRYPOINT ["go", "generate", "./..."]