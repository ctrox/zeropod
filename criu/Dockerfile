FROM debian:12 as build
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    git build-essential libprotobuf-dev libprotobuf-c-dev \
    protobuf-c-compiler protobuf-compiler python3-protobuf \
    libcap-dev libnl-3-dev libnet-dev pkg-config curl ca-certificates \
    gnutls-dev uuid-dev automake texinfo

WORKDIR /workspace
RUN git clone https://github.com/checkpoint-restore/criu.git
WORKDIR /workspace/criu
ADD criu/*.patch .
ARG CRIU_VERSION
RUN test -n "$CRIU_VERSION"
RUN git checkout $CRIU_VERSION
RUN git apply *.patch
RUN make -j $(nproc)

# copy all libraries that criu needs except libc as replacing libc is not that
# straight-forward.
RUN mkdir criu-libs/ && \
    for l in $(ldd criu/criu | awk '{ print $3 }'); do cp $l criu-libs/; done && \
    rm criu-libs/libc.so*

WORKDIR /workspace
# we need to patch gnu tar for so it uses `sh` instead of `/bin/sh` as `/bin/sh`
# is very restrictive in bottlerocket.
RUN git clone https://github.com/Distrotech/tar.git
WORKDIR /workspace/tar
ADD criu/tar/*.patch .
RUN git apply *.patch
RUN autoreconf && FORCE_UNSAFE_CONFIGURE=1 ./configure
RUN make -j $(nproc)

WORKDIR /workspace
RUN git clone https://github.com/lz4/lz4.git
WORKDIR /workspace/lz4
RUN git checkout v1.10.0 && make -j $(nproc)

FROM rust:latest as rust
WORKDIR /workspace
RUN git clone https://github.com/checkpoint-restore/criu-image-streamer.git
WORKDIR /workspace/criu-image-streamer
RUN git checkout 075a7a5eb45cf1f093cda81f8ecb66da4de615f4 && make

# we just add libs and binaries from scratch so this image can be used with
# containerd's client.Install.
FROM scratch AS export-stage
COPY --from=build /workspace/criu/criu/criu /bin/
COPY --from=build /workspace/criu/criu-libs /lib/
COPY --from=build /workspace/tar/src/tar /bin/
COPY --from=build /usr/bin/gzip /bin/
COPY --from=build /bin/sh /bin/
COPY --from=build /workspace/lz4/lz4 /bin/
COPY --from=rust /workspace/criu-image-streamer/target/release/criu-image-streamer /bin/
