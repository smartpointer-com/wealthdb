# wealthdb/deb-build — where the Debian package is built
# (shared/deb/build.sh): the oldest Ubuntu the package supports, with
# Ubuntu's own Java 21 and Python, so ./configure records them at the
# paths the package's dependencies install on every later release; a C
# compiler and C++ library for the engine's cgo; and the Go of GO_IMAGE,
# the image the engine builds FROM (wealthdb/Dockerfile), copied in. Go's
# own programs are static, so they run on any glibc; the engine links
# this release's glibc, the oldest, so it runs on the newer ones too.
# It holds no wealthdb code: a build copies a tree in.
#
# CROSS names more packages: on an arm64 machine, the x86-64 cross
# compilers (gcc-x86-64-linux-gnu, g++-x86-64-linux-gnu), with which the
# engine is built for x86-64 without running anything translated.
ARG GO_IMAGE
ARG UBUNTU=ubuntu:22.04
FROM ${GO_IMAGE} AS go

FROM ${UBUNTU}
ARG CROSS=
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl dpkg-dev g++ gcc git libc6-dev make openjdk-21-jre-headless python3 $CROSS && \
    rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
# A build is not root, and works here.
RUN mkdir -m 1777 /work
WORKDIR /work
