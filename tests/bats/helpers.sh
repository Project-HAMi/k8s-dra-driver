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
    wait_for_kubelet_plugin
}

# Wait for the kubelet plugin rollout and for its devices to be published.
wait_for_kubelet_plugin() {
    kubectl rollout status -n "${DRIVER_NAMESPACE}" daemonset/hami-dra-driver-kubelet-plugin --timeout=5m
    # The DaemonSet is ready before the plugin publishes its first
    # ResourceSlice, so wait for the devices too.
    local deadline=$((SECONDS + 120))
    until [[ -n "$(hami_devices)" ]]; do
        (( SECONDS < deadline )) || { echo "no ${DRIVER_NAME} devices published" >&2; return 1; }
        sleep 2
    done
}

node_exec() {
    docker exec "${KIND_CLUSTER_NAME}-control-plane" "$@"
}

# Mokka's nvidia-smi on the kind node. Its output is the ground truth that the
# driver's view is compared against.
node_nvidia_smi() {
    node_exec "${TEST_NVIDIA_DRIVER_ROOT}/usr/bin/nvidia-smi" "$@"
}

# The devices this driver publishes, one JSON object per line.
hami_devices() {
    kubectl get resourceslices -o json |
        jq -c --arg driver "${DRIVER_NAME}" '.items[] | select(.spec.driver == $driver) | .spec.devices[]?'
}

# claim_gpu_uuids CLAIM
#
# The UUIDs of the GPUs allocated to CLAIM, one per line.
claim_gpu_uuids() {
    local devices
    devices=$(kubectl get resourceclaim "$1" -o json |
        jq -c --arg driver "${DRIVER_NAME}" '[.status.allocation.devices.results[] | select(.driver == $driver) | .device]')
    hami_devices | jq -r --argjson names "${devices}" 'select(.name as $n | $names | index($n)) | .attributes.uuid.string'
}

# node_claim_state CLAIM_UID POD_UID
#
# The state the driver keeps on the node for a claim prepared for a pod, one
# path per line: the pod's HAMi-Core cache directories, the claim's CDI spec,
# and the checkpoint while it records the claim. Paths are the chart defaults.
node_claim_state() {
    # shellcheck disable=SC2016 # expanded by the node's shell
    node_exec sh -c '
        ls -d /usr/local/vgpu/containers/"$2"_* /var/run/cdi/*"$1"* 2>/dev/null
        grep -l "$1" "/var/lib/kubelet/plugins/$3/checkpoint.json"
    ' sh "$1" "$2" "${DRIVER_NAME}" || true
}

show_debug_info() {
    echo -e "\nDEBUG INFO START"
    kubectl get resourceclaims,pods -o wide || true
    kubectl describe pods || true
    kubectl logs -n "${DRIVER_NAMESPACE}" -l "${KUBELET_PLUGIN_SELECTOR}" --all-containers --prefix --tail=400 || true
    echo -e "DEBUG INFO END\n"
}
