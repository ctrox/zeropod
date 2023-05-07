FROM ubuntu:20.04 as build
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  git build-essential libprotobuf-dev libprotobuf-c-dev \
  protobuf-c-compiler protobuf-compiler python-protobuf \
  libcap-dev libnl-3-dev libnet-dev pkg-config curl ca-certificates
WORKDIR /workspace
RUN git clone https://github.com/checkpoint-restore/criu.git
WORKDIR /workspace/criu
RUN git checkout v3.18 && make
RUN find / -name libprotobuf-c.so.1

FROM ubuntu

WORKDIR /app

COPY --from=build /workspace/criu/criu/criu /app
COPY --from=build /usr/lib/aarch64-linux-gnu/libprotobuf-c.so.1 /app/lib/
COPY --from=build /lib/aarch64-linux-gnu/libnl-3.so.200 /app/lib/
COPY --from=build /usr/lib/aarch64-linux-gnu/libnet.so.1 /app/lib/

