# The Linux build environment for Signal Atlas, used by scripts/release.py on any PC with Docker.
#
# Ubuntu 22.04 is the oldest system the build supports: the binary links against its glibc 2.35
# and WebKitGTK 4.1, so it runs on Ubuntu 22.04 and newer, Debian 12 and newer, Fedora 39 and newer,
# and other distributions of that age.
FROM ubuntu:22.04

ARG GO_VERSION=1.27.3
ARG TARGETARCH=amd64
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl build-essential pkg-config dpkg-dev \
        libgtk-3-dev libwebkit2gtk-4.1-dev \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH \
    GOFLAGS=-buildvcs=false \
    CGO_ENABLED=1
WORKDIR /src
# windres, to compile the Windows icon, manifest and version information into a .syso, and zip
RUN apt-get update && apt-get install -y --no-install-recommends binutils-mingw-w64-x86-64 zip \
    && rm -rf /var/lib/apt/lists/*
