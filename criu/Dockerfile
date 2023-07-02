FROM fedora:38 as build

RUN dnf install -y \
  diffutils \
  findutils \
  gcc \
  git \
  gnutls-devel \
  gzip \
  iproute \
  iptables \
  nftables \
  nftables-devel \
  libaio-devel \
  libasan \
  libcap-devel \
  libnet-devel \
  libnl3-devel \
  libbsd-devel \
  make \
  procps-ng \
  protobuf-c-devel \
  protobuf-devel \
  python3-flake8 \
  python3-PyYAML \
  python3-future \
  python3-protobuf \
  python3-junit_xml \
  python3-pip \
  python3-importlib-metadata \
  python-unversioned-command \
  redhat-rpm-config \
  sudo \
  tar \
  which \
  e2fsprogs \
  rubygem-asciidoctor \
  kmod

WORKDIR /workspace

RUN git clone https://github.com/checkpoint-restore/criu.git
WORKDIR /workspace/criu

ARG CRIU_VERSION=v3.18
RUN git checkout $CRIU_VERSION
RUN make -j $(nproc)

FROM scratch AS export-stage
COPY --from=build /workspace/criu/criu/criu /bin/
COPY --from=build /usr/lib64/libprotobuf-c.so.1 /lib/
COPY --from=build /usr/lib64/libnl-3.so.200 /lib/
COPY --from=build /usr/lib64/libnet.so.1 /lib/
COPY --from=build /usr/lib64/libnftables.so.1 /lib/
COPY --from=build /usr/lib64/libjansson.so.4 /lib/
COPY --from=build /usr/lib64/libffi.so.8 /lib/
COPY --from=build /usr/lib64/libedit.so.0 /lib/
COPY --from=build /usr/lib64/libnftnl.so.11 /lib/