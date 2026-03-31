BUILD_MULTI_ARCH_IMAGES ?= no
DOCKER ?= docker
REGCTL ?= regctl
BUILDX =
PUSH_ON_BUILD ?= false

##### Global variables #####
include $(CURDIR)/versions.mk

ifeq ($(IMAGE_NAME),)
IMAGE_NAME := $(REGISTRY)/$(DRIVER_NAME)
endif

# Note: this is sometimes a v-prefixed version string
# (in case of proper releases) or sometimes a commit hash
# (in case of regular CI builds) in which case VERSION
# is not v-prefixed.
IMAGE_VERSION := $(VERSION)

IMAGE_TAG ?= $(IMAGE_VERSION)
IMAGE = $(IMAGE_NAME):$(IMAGE_TAG)

##### Public rules #####
.PHONY: build

ifeq ($(BUILD_MULTI_ARCH_IMAGES),true)
BUILDX = buildx
DOCKER_BUILD_OPTIONS = --output=type=image,push=$(PUSH_ON_BUILD)
DOCKER_BUILD_PLATFORM_OPTIONS = --platform=linux/amd64,linux/arm64
else
# We also support building single-platform images.
# These are mostly used for local testing and as such we don't allow these to
# be pushed on build.
ifeq ($(PUSH_ON_BUILD),true)
$(error PUSH_ON_BUILD=true is not supported for single architecture builds)
endif
ARCH ?= $(shell uname -m | sed -e 's,aarch64,arm64,' -e 's,x86_64,amd64,')
DOCKER_BUILD_PLATFORM_OPTIONS = --platform=linux/$(ARCH)
endif

DOCKERFILE = $(CURDIR)/deploy/container/Dockerfile

# Build the container image
build:
	DOCKER_BUILDKIT=1 \
		$(DOCKER) $(BUILDX) build --pull \
		--provenance=false --sbom=false \
		$(DOCKER_BUILD_OPTIONS) \
		$(DOCKER_BUILD_PLATFORM_OPTIONS) \
		--tag $(IMAGE) \
		--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
		--build-arg BASH_STATIC_GIT_REF="$(BASH_STATIC_GIT_REF)" \
		--build-arg TOOLKIT_CONTAINER_IMAGE="$(TOOLKIT_CONTAINER_IMAGE)" \
		--build-arg HAMI_CORE_BUILD_IMAGE="$(HAMI_CORE_BUILD_IMAGE)" \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--progress=plain \
		-f $(DOCKERFILE) \
		$(CURDIR)
