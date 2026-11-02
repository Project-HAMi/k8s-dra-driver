# shellcheck shell=bash
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

# Helpers shared by the test files. The suite runs against the cluster of the
# current kubectl context; hack/ci/mock-nvml/e2e-test.sh sets one up.

DRIVER_NAME="hami-core-gpu.project-hami.io"
DRIVER_NAMESPACE="hami-dra-driver"
DRIVER_RELEASE="hami-dra-driver"
KUBELET_PLUGIN_SELECTOR="hami-dra-driver-component=kubelet-plugin"

# The chart under test: the local chart, which has no version to pin.
TEST_CHART_REPO="${TEST_CHART_REPO:-chart/hami-dra-driver}"
TEST_CHART_VERSION="${TEST_CHART_VERSION:-}"

_common_setup() {
    bats_load_library bats-support
    bats_load_library bats-assert
}

# iupgrade_wait CHART VERSION [HELM_ARGS...]
#
# Install or upgrade the driver from CHART and wait for the kubelet plugin to
# be ready. VERSION is empty for a local chart. TEST_IMAGE replaces the image
# only for the chart under test, so a published release, such as the one an
# upgrade test starts from, keeps its own image. Same arguments as upstream's
# helper, except that the Helm arguments are passed directly.
iupgrade_wait() {
    local chart="$1" version="$2"
    shift 2

    local args=()
    if [[ -n "${version}" ]]; then
        args+=(--version "${version}")
    fi
    if [[ "${chart}" == "${TEST_CHART_REPO}" && "${version}" == "${TEST_CHART_VERSION}" ]]; then
        args+=(--set-string image.repository="${TEST_IMAGE%:*}"
            --set-string image.tag="${TEST_IMAGE##*:}")
    fi

    helm upgrade --install "${DRIVER_RELEASE}" "${chart}" \
        --namespace "${DRIVER_NAMESPACE}" --create-namespace \
        --set nvidiaDriverRoot="${TEST_NVIDIA_DRIVER_ROOT}" \
        --wait --timeout 5m "${args[@]}" "$@"
    # Helm 3 does not wait for a DaemonSet whose maxUnavailable covers every pod.
    kubectl rollout status -n "${DRIVER_NAMESPACE}" daemonset/hami-dra-driver-kubelet-plugin --timeout=5m
}

# Mokka's nvidia-smi on the kind node. Its output is the ground truth that the
# driver's view is compared against.
node_nvidia_smi() {
    docker exec "${KIND_CLUSTER_NAME}-control-plane" "${TEST_NVIDIA_DRIVER_ROOT}/usr/bin/nvidia-smi" "$@"
}

# The devices this driver publishes, one JSON object per line.
hami_devices() {
    kubectl get resourceslices -o json |
        jq -c --arg driver "${DRIVER_NAME}" '.items[] | select(.spec.driver == $driver) | .spec.devices[]?'
}

show_debug_info() {
    echo -e "\nDEBUG INFO START"
    kubectl get resourceclaims,pods -o wide || true
    kubectl describe pods || true
    kubectl logs -n "${DRIVER_NAMESPACE}" -l "${KUBELET_PLUGIN_SELECTOR}" --all-containers --prefix --tail=400 || true
    echo -e "DEBUG INFO END\n"
}
