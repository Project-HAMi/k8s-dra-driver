# syntax=docker.io/docker/dockerfile:1.7-labs
# The above enables the COPY --exclude + --parent flags
#
# Copyright (c) 2019-2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG TOOLKIT_CONTAINER_IMAGE=unknown

FROM --platform=$BUILDPLATFORM nvidia/cuda:12.3.2-devel-ubuntu20.04 AS hami-core-build

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get -y update; apt-get -y install cmake
COPY . /k8s-dra-driver
WORKDIR /k8s-dra-driver/lib/hami-core
RUN bash ./build.sh

# Add hami-lib into distroless image
RUN chmod +x /k8s-dra-driver/lib/nvidia/vgpu-init.sh
RUN mkdir -p /k8s-dra-driver/lib/local/lib/hami
RUN mv /k8s-dra-driver/lib/hami-core/build/libvgpu.so /k8s-dra-driver/lib/local/lib/hami
RUN mv /k8s-dra-driver/lib/nvidia/ld.so.preload /k8s-dra-driver/lib/local/lib/hami

# Run build with binaries native to the current build platform.
FROM --platform=$BUILDPLATFORM nvcr.io/nvidia/base/ubuntu:jammy-20251013 AS build

# Require arg to be provided (set invalid default value).
ARG GOLANG_VERSION=x.x.x

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
        curl \
        time \
        make \
        gcc \
        gcc-aarch64-linux-gnu \
        gcc-x86-64-linux-gnu && \
        rm -rf /var/lib/apt/lists/* /var/cache/apt/archives

RUN curl -sSfL --retry 8 --retry-all-errors --connect-timeout 10 --retry-delay 5 \
        https://go.dev/dl/go${GOLANG_VERSION}.linux-${BUILDARCH}.tar.gz | tar -C /usr/local -xz

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

# Build static bash binary (against musl). Notes: cross-compilation isn't robust
# (see non-deterministic segfaults tracked in issues/502; source code download
# instabilities tracked in issues/743) and generally takes signifiant time even
# on happy path (dominiates CI duration as of the time of writing). Do not build
# this every time. Instead, consume it from a previous image on GHCR -- also,
# leave machinery (out-commented) in place.
#
# Choose: 1) fresh bash build or 2) use binary from previous image
#
# option 1:
# WORKDIR /bashbuild
# COPY hack/build-bash-static.sh .
# RUN export BASH_STATIC_GIT_REF && bash /bashbuild/build-bash-static.sh
# RUN cd /bashbuild/bash-static/releases && ./bash*-static --version && mv bash-*-static /bashbuild/bash && file /bashbuild/bash

# option 2:
FROM ghcr.io/nvidia/k8s-dra-driver-gpu:v25.12.0-dev-839e966a AS bash

FROM ${TOOLKIT_CONTAINER_IMAGE} AS toolkit

FROM nvcr.io/nvidia/distroless/cc:v4.0.1-dev

ENV NVIDIA_DISABLE_REQUIRE="true"
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=utility

LABEL io.k8s.display-name="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL name="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL summary="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL description="HAMi-Core DRA Driver for NVIDIA GPUs"
LABEL org.opencontainers.image.description="HAMI-Core DRA Driver for NVIDIA GPUs"
LABEL org.opencontainers.image.source="https://github.com/Project-HAMi/k8s-dra-driver"


# option 1: fresh bash build
# COPY --from=build   /bashbuild/bash                           /bin/bash

# option 2: bash from previous image
COPY --from=bash    /bin/bash                                 /bin/bash

COPY --from=toolkit /artifacts/rpm/usr/bin/nvidia-cdi-hook   /usr/bin/nvidia-cdi-hook
# COPY --from=build   /artifacts/compute-domain-controller      /usr/bin/compute-domain-controller
# COPY --from=build   /artifacts/compute-domain-kubelet-plugin  /usr/bin/compute-domain-kubelet-plugin
# COPY --from=build   /artifacts/compute-domain-daemon          /usr/bin/compute-domain-daemon
# COPY --from=build   /artifacts/gpu-kubelet-plugin             /usr/bin/gpu-kubelet-plugin
# COPY --from=build   /artifacts/webhook                        /usr/bin/webhook
#
COPY --from=build   /build/scripts/bind_to_driver.sh         /usr/bin/bind_to_driver.sh
COPY --from=build   /build/scripts/unbind_from_driver.sh     /usr/bin/unbind_from_driver.sh
COPY /hack/kubelet-plugin-prestart.sh /usr/bin/kubelet-plugin-prestart.sh

COPY --from=build   /artifacts/hami-kubelet-plugin           /usr/bin/hami-kubelet-plugin

COPY --from=hami-core-build /k8s-dra-driver/lib/local /usr/local
COPY --from=hami-core-build /k8s-dra-driver/lib/nvidia/vgpu-init.sh /usr/bin/vgpu-init.sh

COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /etc/group /etc/group
USER root:root

# Smoke-test executables (provide early build feedback).
RUN ["/usr/bin/hami-kubelet-plugin", "--version"]
RUN ["/bin/bash", "--version"]

ENTRYPOINT ["/bin/bash"]
