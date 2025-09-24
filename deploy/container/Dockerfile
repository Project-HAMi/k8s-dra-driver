ARG TOOLKIT_CONTAINER_IMAGE=unknown

# Run build with binaries native to the current build platform.
FROM --platform=$BUILDPLATFORM nvcr.io/nvidia/base/ubuntu:jammy-20250619 AS build

# Require arg to be provided (set invalid default value).
ARG GOLANG_VERSION=x.x.x
ARG BASH_STATIC_GIT_REF=unknown

# BUILDARCH, TARGETARCH (and others) are defined in the global scope by
# BuiltKit. BUILDARCH is the architecture of the build platform. TARGETARCH is
# set via the --platform arg provided to the `docker buildx build ...` command.
# Redefining those variables here without new values makes the outer-context
# values available to in-stage RUN commands. Arch values are of the form
# amd64/arm64.
ARG BUILDARCH
ARG TARGETARCH

# Install dependencies for Go build. Do not clear apt cache (does not
# leak into prod stage).
RUN apt-get update && \
    apt-get install -y \
        wget \
        make \
        git \
        gcc-aarch64-linux-gnu \
        gcc

# Install dependencies for `bash-static` build.
RUN apt-get install -y gpg curl autoconf file

# Build static bash binary (against musl).
WORKDIR /bashbuild
RUN git clone https://github.com/robxu9/bash-static/

# Note: cross-compilation in `build.sh` isn't robust. Fix that
# by manually setting CC, and also strip command must conditionally
# be replaced by an aarch64-specific version.
RUN ARCH="$TARGETARCH" && STRIP=strip && \
    [ "$ARCH" = "arm64" ] && ARCH="aarch64" || true && \
    [ "$ARCH" = "amd64" ] && ARCH="x86_64" || true && \
    [ "$ARCH" = "aarch64" ] && STRIP="aarch64-linux-gnu-strip" && CC="aarch64-linux-gnu-gcc" || true && \
    echo "detected arch: $ARCH" && \
    echo "cc to use: $CC" && \
    echo "strip to use: $STRIP" && \
    cd bash-static && git checkout ${BASH_STATIC_GIT_REF} && \
    sed -i 's|https://ftp\.gnu\.org/gnu|https://ftpmirror.gnu.org/|g' ./build.sh && \
    sed -i 's/-sLO/-sSfLO --retry 300 --connect-timeout 20 --retry-delay 2/g' ./build.sh && \
    sed -i 's/strip/$STRIP/g' ./build.sh && \
    sed -i 's/make -s \&\& make -s tests/make -j4/g' ./build.sh && \
    bash version-52.sh && STRIP=$STRIP CC=$CC ./build.sh linux $ARCH

# With above's commit, this emits
# 'GNU bash, version 5.2.37(1)-release (aarch64-unknown-linux-musl)'
RUN cd /bashbuild/bash-static/releases && ./bash*-static --version
RUN mv /bashbuild/bash-static/releases/bash-*-static /bashbuild/bash
# Capture actual file architecture in the build output.
RUN file /bashbuild/bash

RUN wget -nv -O - https://storage.googleapis.com/golang/go${GOLANG_VERSION}.linux-${BUILDARCH}.tar.gz \
    | tar -C /usr/local -xz

ENV GOPATH=/go
ENV PATH=$GOPATH/bin:/usr/local/go/bin:$PATH

WORKDIR /build

# Copy everything that is needed for the go build, but do not invalidate go
# build artifact layer unnecessarily often. Note that --exclude only affects the
# cache.
# COPY    --exclude=./templates/** \
#         --exclude=./deployments/helm/** \
#         --exclude=./hack/kubelet-plugin-prestart.sh \
#         --exclude=./demo/** \
#         --exclude=./Dockerfile \
#         . .
COPY . .

RUN mkdir /artifacts

# The VERSION and GIT_COMMIT env vars are consumed by the `make` target below.
ARG VERSION="N/A"
ARG GIT_COMMIT="unknown"
RUN if [ "$TARGETARCH" = "amd64" ]; then \
        cc=gcc; \
    elif [ "$TARGETARCH" = "arm64" ]; then \
        cc=aarch64-linux-gnu-gcc; \
    fi && \
    make CC=${cc} GOARCH=${TARGETARCH} PREFIX=/artifacts cmds

FROM ${TOOLKIT_CONTAINER_IMAGE} AS toolkit

FROM nvcr.io/nvidia/distroless/cc:v3.1.12-dev

ENV NVIDIA_DISABLE_REQUIRE="true"
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=utility

LABEL io.k8s.display-name="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL name="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL summary="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL description="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL org.opencontainers.image.description="HAMI-Core DRA Driver for NVIDIA GPUs"
LABEL org.opencontainers.image.source="https://github.com/Project-HAMi/k8s-dra-driver"

COPY --from=toolkit /artifacts/rpm/usr/bin/nvidia-cdi-hook   /usr/bin/nvidia-cdi-hook
COPY --from=build   /bashbuild/bash                          /bin/bash
COPY --from=build   /artifacts/hami-kubelet-plugin           /usr/bin/hami-kubelet-plugin

COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /etc/group /etc/group
USER root:root

# Smoke-test executables (provide early build feedback).
RUN ["/usr/bin/hami-kubelet-plugin", "--version"]
RUN ["/bin/bash", "--version"]
