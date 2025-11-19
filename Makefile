# Copyright 2025 The HAMi Authors.
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


DOCKER   ?= docker
MKDIR    ?= mkdir
TR       ?= tr
CC       ?= cc
DIST_DIR ?= $(CURDIR)/dist

include $(CURDIR)/common.mk
include $(CURDIR)/versions.mk

.NOTPARALLEL:

ifeq ($(IMAGE_NAME),)
IMAGE_NAME = $(REGISTRY)/$(DRIVER_NAME)
endif

CMDS := $(patsubst ./cmd/%/,%,$(sort $(dir $(wildcard ./cmd/*/))))
CMD_TARGETS := $(patsubst %,cmd-%, $(CMDS))

CHECK_TARGETS := golangci-lint check-generate
MAKE_TARGETS := binaries build build-image check fmt lint-internal test examples cmds coverage generate vendor check-modules $(CHECK_TARGETS)

TARGETS := $(MAKE_TARGETS) $(CMD_TARGETS)

DOCKER_TARGETS := $(patsubst %,docker-%, $(TARGETS))
.PHONY: $(TARGETS) $(DOCKER_TARGETS)

GOOS ?= linux
GOARCH ?= $(shell uname -m | sed -e 's,aarch64,arm64,' -e 's,x86_64,amd64,')
ifeq ($(VERSION),)
CLI_VERSION = $(LIB_VERSION)$(if $(LIB_TAG),-$(LIB_TAG))
else
CLI_VERSION = $(VERSION)
endif

binaries: cmds
ifneq ($(PREFIX),)
cmd-%: COMMAND_BUILD_OPTIONS = -o $(PREFIX)/$(*)
endif
cmds: $(CMD_TARGETS)
# TODO: Get the version from version.mk
$(CMD_TARGETS): cmd-%:
	CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' \
		CC=$(CC) CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -ldflags "-s -w -X github.com/NVIDIA/k8s-dra-driver-gpu/internal/info.version=${vNVVERSION} -X github.com/Project-HAMi/k8s-dra-driver/pkg/version.nvversion=${NVVERSION} -X github.com/Project-HAMi/k8s-dra-driver/pkg/version.version=${VERSION}" \
		$(COMMAND_BUILD_OPTIONS) $(MODULE)/cmd/$(*)

build:
	CC=$(CC) GOOS=$(GOOS) GOARCH=$(GOARCH) go build ./...

# Generate an image for containerized builds
# Note: This image is local only
.PHONY: .image image
image: .image
.image:
	make -f deploy/container/Makefile build
