# 20.04 seems to be the best compromise for compatibility with most systems.
# For example when building on 22.04, glibc is too recent to work on GKE. It
# would be better to have a fully static build of criu but so far that has
# been a disappointingly unsuccessful endeavour.
FROM ubuntu:20.04 as build
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  git build-essential libprotobuf-dev libprotobuf-c-dev \
  protobuf-c-compiler protobuf-compiler python-protobuf \
  libcap-dev libnl-3-dev libnet-dev pkg-config curl ca-certificates

WORKDIR /workspace

RUN git clone https://github.com/checkpoint-restore/criu.git
WORKDIR /workspace/criu
ARG CRIU_VERSION=v3.18
RUN git checkout $CRIU_VERSION

RUN make -j $(nproc)
RUN mv ./criu/criu ./criu/criu-iptables

# build with nftables support
RUN DEBIAN_FRONTEND=noninteractive apt-get install -y libnftables-dev
RUN make clean && make -j $(nproc)

# we just add libs and binaries from scratch so this image can be used with
# containerd's client.Install.
FROM scratch AS export-stage
COPY --from=build /workspace/criu/criu/criu /bin/
COPY --from=build /workspace/criu/criu/criu-iptables /bin/
COPY --from=build /usr/lib/*-linux-gnu/libprotobuf-c.so.1 /lib/
COPY --from=build /lib/*-linux-gnu/libnl-3.so.200 /lib/
COPY --from=build /usr/lib/*-linux-gnu/libnet.so.1 /lib/
COPY --from=build /lib/*-linux-gnu/libnftables.so.1 /lib/
COPY --from=build /lib/*-linux-gnu/libnftnl.so.11 /lib/
COPY --from=build /lib/*-linux-gnu/libjansson.so.4 /lib/
