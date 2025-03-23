FROM debian:12 as build
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    git build-essential libprotobuf-dev libprotobuf-c-dev \
    protobuf-c-compiler protobuf-compiler python3-protobuf \
    libcap-dev libnl-3-dev libnet-dev pkg-config curl ca-certificates \
    gnutls-dev uuid-dev

WORKDIR /workspace

RUN git clone https://github.com/checkpoint-restore/criu.git
WORKDIR /workspace/criu
ADD criu/*.patch .
ARG CRIU_VERSION=v4.0
RUN git checkout $CRIU_VERSION
RUN git apply *.patch
RUN make -j $(nproc)

# copy all libraries that criu needs except libc as replacing libc is not that
# straight-forward.
RUN mkdir criu-libs/ && \
    for l in $(ldd criu/criu | awk '{ print $3 }'); do cp $l criu-libs/; done && \
    rm criu-libs/libc.so*

# we just add libs and binaries from scratch so this image can be used with
# containerd's client.Install.
FROM scratch AS export-stage
COPY --from=build /workspace/criu/criu/criu /bin/
COPY --from=build /workspace/criu/criu-libs /lib/
