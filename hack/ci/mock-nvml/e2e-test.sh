#!/usr/bin/env bash

# Copyright 2026 The HAMi Authors.
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

# GPU-less E2E tests. Creates a kind cluster, installs Mokka to mock the NVIDIA
# driver and GPUs, builds the driver image from this tree, and runs the BATS
# suite under tests/bats, which installs the driver with its Helm chart.
#
# Requires docker, git, kind, kubectl, helm and jq, plus make and go to build
# the image.
#
# Environment:
#   KIND_CLUSTER_NAME  kind cluster to create (hami-e2e)
#   RECREATE_CLUSTER   delete an existing cluster of that name instead of failing (false)
#   KIND_NODE_IMAGE    Kubernetes version under test, the oldest supported (kindest/node:v1.34.2)
#   MOKKA_VERSION      Mokka (nvml-mock) chart version (0.5.1)
#   GPU_PROFILE        Mokka GPU profile (t4)
#   TEST_IMAGE         driver image under test (projecthami/k8s-dra-driver:e2e)
#   SKIP_IMAGE_BUILD   use the local TEST_IMAGE as is (false)
#   SKIP_CLEANUP       keep the cluster when done, e.g. to rerun one test (false)
#   ARTIFACTS          where logs are written when the run fails (/tmp/hami-e2e-artifacts)

set -o errexit -o nounset -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

: "${KIND_CLUSTER_NAME:=hami-e2e}"
: "${RECREATE_CLUSTER:=false}"
: "${KIND_NODE_IMAGE:=kindest/node:v1.34.2}"
: "${MOKKA_VERSION:=0.5.1}"
: "${GPU_PROFILE:=t4}"
: "${TEST_IMAGE:=projecthami/k8s-dra-driver:e2e}"
: "${SKIP_IMAGE_BUILD:=false}"
: "${SKIP_CLEANUP:=false}"
: "${ARTIFACTS:=/tmp/hami-e2e-artifacts}"

# Pinned so that local runs and CI run the same suite runner.
BATS_CORE_VERSION=v1.14.0
BATS_SUPPORT_VERSION=v0.3.0
BATS_ASSERT_VERSION=v2.2.4
BATS_DIR="/tmp/hami-e2e-bats-${BATS_CORE_VERSION}"

export KIND_CLUSTER_NAME TEST_IMAGE
# Where Mokka stages the mock driver tree on every node.
export TEST_NVIDIA_DRIVER_ROOT=/var/lib/nvml-mock/driver

log() {
    echo "[$(date -u +%H:%M:%S)] $*"
}

install_bats() {
    # The marker is written last, so an interrupted install is redone.
    if [[ ! -f "${BATS_DIR}/.installed" ]]; then
        rm -rf "${BATS_DIR}"
        git -c advice.detachedHead=false clone -q --depth 1 --branch "${BATS_CORE_VERSION}" https://github.com/bats-core/bats-core "${BATS_DIR}/core"
        git -c advice.detachedHead=false clone -q --depth 1 --branch "${BATS_SUPPORT_VERSION}" https://github.com/bats-core/bats-support "${BATS_DIR}/lib/bats-support"
        git -c advice.detachedHead=false clone -q --depth 1 --branch "${BATS_ASSERT_VERSION}" https://github.com/bats-core/bats-assert "${BATS_DIR}/lib/bats-assert"
        touch "${BATS_DIR}/.installed"
    fi
    export PATH="${BATS_DIR}/core/bin:${PATH}"
    export BATS_LIB_PATH="${BATS_DIR}/lib"
}

collect_artifacts() {
    log "collecting logs into ${ARTIFACTS}"
    mkdir -p "${ARTIFACTS}"
    {
        kubectl get nodes,pods -A -o wide
        kubectl get deviceclasses,resourceslices,resourceclaims -A -o yaml
        kubectl get events -A --sort-by=.lastTimestamp
    } >"${ARTIFACTS}/cluster.txt" 2>&1 || true
    kind export logs "${ARTIFACTS}/kind" --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
}

cleanup() {
    local rc=$?
    if [[ ${rc} -ne 0 ]]; then
        collect_artifacts
    fi
    if [[ "${SKIP_CLEANUP}" != "true" ]]; then
        kind delete cluster --name "${KIND_CLUSTER_NAME}" || true
    fi
    exit "${rc}"
}

tools=(docker git kind kubectl helm jq)
if [[ "${SKIP_IMAGE_BUILD}" != "true" ]]; then
    # versions.mk runs go to find the toolkit image.
    tools+=(make go)
fi
for tool in "${tools[@]}"; do
    command -v "${tool}" >/dev/null || { echo "${tool} is required" >&2; exit 1; }
done

# Checked before the build, which takes minutes. Local runs may share the name
# with a cluster still in use, so it is only deleted when asked. The list is
# read in full first: grep -q in a pipe can kill kind with SIGPIPE, which
# pipefail would turn into "not found".
clusters="$(kind get clusters)"
if grep -qx "${KIND_CLUSTER_NAME}" <<<"${clusters}"; then
    if [[ "${RECREATE_CLUSTER}" != "true" ]]; then
        echo "kind cluster ${KIND_CLUSTER_NAME} exists; delete it or set RECREATE_CLUSTER=true" >&2
        exit 1
    fi
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
fi
install_bats

if [[ "${SKIP_IMAGE_BUILD}" != "true" ]]; then
    log "building ${TEST_IMAGE}"
    make -C "${REPO_ROOT}" -f deploy/container/Makefile build \
        IMAGE_NAME="${TEST_IMAGE%:*}" IMAGE_TAG="${TEST_IMAGE##*:}" DOCKER_BUILD_OPTIONS=--load
fi

log "creating kind cluster ${KIND_CLUSTER_NAME} (${KIND_NODE_IMAGE})"
kind create cluster --name "${KIND_CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" \
    --config "${SCRIPT_DIR}/kind.yaml" --wait 2m
# Set only once the cluster is ours: a failed create removes its own nodes,
# and the name may belong to a cluster created by someone else meanwhile.
trap cleanup EXIT
kind load docker-image "${TEST_IMAGE}" --name "${KIND_CLUSTER_NAME}"

log "installing Mokka ${MOKKA_VERSION} (${GPU_PROFILE})"
helm install nvml-mock oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock \
    --version "${MOKKA_VERSION}" --namespace mokka --create-namespace \
    --set gpu.profile="${GPU_PROFILE}" --wait --timeout 5m >/dev/null
# Helm 3 does not wait for a DaemonSet whose maxUnavailable covers every pod.
kubectl rollout status -n mokka daemonset/nvml-mock --timeout=5m

log "running tests"
cd "${REPO_ROOT}"
bats --print-output-on-failure --timing tests/bats/
