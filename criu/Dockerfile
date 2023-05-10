FROM ubuntu:20.04 as build
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  git build-essential libprotobuf-dev libprotobuf-c-dev \
  protobuf-c-compiler protobuf-compiler python-protobuf \
  libcap-dev libnl-3-dev libnet-dev pkg-config curl ca-certificates
WORKDIR /workspace

RUN git clone https://github.com/checkpoint-restore/criu.git
WORKDIR /workspace/criu

RUN git checkout v3.18
RUN make -j $(nproc)

FROM scratch AS export-stage
COPY --from=build /workspace/criu/criu/criu /
COPY --from=build /usr/lib/x86_64-linux-gnu/libprotobuf-c.so.1 /lib/
COPY --from=build /lib/x86_64-linux-gnu/libnl-3.so.200 /lib/
COPY --from=build /usr/lib/x86_64-linux-gnu/libnet.so.1 /lib/
