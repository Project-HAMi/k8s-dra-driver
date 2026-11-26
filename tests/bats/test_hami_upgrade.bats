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

bats_require_minimum_version 1.5.0

setup_file() {
    load 'helpers.sh'
    # v0.2.0 has no published chart, so take it from the release's source.
    curl -fsSL "https://github.com/Project-HAMi/k8s-dra-driver/archive/refs/tags/${TEST_CHART_LASTSTABLE_VERSION}.tar.gz" |
        tar -xz -C "${BATS_FILE_TMPDIR}"
    LASTSTABLE_CHART=$(echo "${BATS_FILE_TMPDIR}"/*/chart/hami-dra-driver)
    export LASTSTABLE_CHART
}

setup() {
    load 'helpers.sh'
    _common_setup
}

teardown() {
    kubectl delete pods,resourceclaims,resourceclaimtemplates -l env=batssuite --ignore-not-found --timeout=60s
}

bats::on_failure() {
    show_debug_info
}

kubelet_plugin_image() {
    kubectl get daemonset -n "${DRIVER_NAMESPACE}" hami-dra-driver-kubelet-plugin \
        -o jsonpath='{.spec.template.spec.containers[0].image}'
}

# The ELF machine of the HAMi-Core library that the driver installed on the
# node, and the one that the node needs, as hex: 3e for x86-64, b7 for arm64.
node_libvgpu_machine() {
    node_exec sh -c 'od -An -tx1 -j18 -N1 /usr/local/vgpu/libvgpu.so | tr -d " \n"'
}

node_machine() {
    case "$(node_exec uname -m)" in
    x86_64) echo 3e ;;
    aarch64) echo b7 ;;
    esac
}

# bats test_tags=fastfeedback
@test "HAMi: upgrade from the last release keeps a running pod, and cleans up after it" {
    # Start from a node without the driver or its state, as on a fresh install.
    helm uninstall "${DRIVER_RELEASE}" -n "${DRIVER_NAMESPACE}" --ignore-not-found --wait --timeout 2m
    kubectl wait --for=delete pods -n "${DRIVER_NAMESPACE}" -l "${KUBELET_PLUGIN_SELECTOR}" --timeout=60s
    node_exec rm -rf "/var/lib/kubelet/plugins/${DRIVER_NAME}"

    # The release's chart defaults to an older image tag, so set its own.
    iupgrade_wait "${LASTSTABLE_CHART}" "" --set-string image.tag="${TEST_CHART_LASTSTABLE_VERSION}"
    assert_equal "$(kubelet_plugin_image)" "projecthami/k8s-dra-driver:${TEST_CHART_LASTSTABLE_VERSION}"
    # The published v0.2.0 arm64 image ships an x86-64 libvgpu.so, which
    # cannot limit pods on an arm64 node.
    if [[ "$(node_libvgpu_machine)" != "$(node_machine)" ]]; then
        skip "${TEST_CHART_LASTSTABLE_VERSION} installs a libvgpu.so for another architecture"
    fi

    local _podname="pod-template-gpu"
    kubectl apply -f tests/bats/specs/template-pod.yaml
    kubectl wait --for=condition=READY pods "${_podname}" --timeout=180s
    local pod_uid claim claim_uid
    pod_uid=$(kubectl get pods "${_podname}" -o jsonpath='{.metadata.uid}')
    claim=$(kubectl get pods "${_podname}" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')
    claim_uid=$(kubectl get resourceclaim "${claim}" -o jsonpath='{.metadata.uid}')
    # Limited by the release, so that a failure below is the upgrade's.
    run --separate-stderr kubectl exec "${_podname}" -- \
        nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "4096"
    run node_claim_state "${claim_uid}" "${pod_uid}"
    assert_line "/usr/local/vgpu/containers/${pod_uid}_ctr"
    assert_line --regexp "^/var/run/cdi/.*${claim_uid}"
    assert_line --partial "checkpoint.json"
    # Shown on failure: the input that the new driver failed to read.
    echo "checkpoint written by ${TEST_CHART_LASTSTABLE_VERSION}:"
    node_exec cat "/var/lib/kubelet/plugins/${DRIVER_NAME}/checkpoint.json"

    iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}"
    assert_equal "$(kubelet_plugin_image)" "${TEST_IMAGE}"

    # The same pod, its container not restarted, and still limited.
    run kubectl get pods "${_podname}" \
        -o jsonpath='{.metadata.uid} {.status.phase} {.status.containerStatuses[0].restartCount}'
    assert_output "${pod_uid} Running 0"
    run --separate-stderr kubectl exec "${_podname}" -- \
        nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "4096"

    # The new driver unprepares the claim that the release prepared.
    kubectl delete pods "${_podname}" --timeout=60s
    kubectl wait --for=delete resourceclaim "${claim}" --timeout=60s
    local deadline=$((SECONDS + 60))
    until [[ -z "$(node_claim_state "${claim_uid}" "${pod_uid}")" ]] || (( SECONDS >= deadline )); do
        sleep 2
    done
    run node_claim_state "${claim_uid}" "${pod_uid}"
    assert_output ""

    # And prepares new claims.
    kubectl apply -f tests/bats/specs/single-pod.yaml
    kubectl wait --for=condition=READY pods pod-single-gpu --timeout=180s
    run --separate-stderr kubectl exec pod-single-gpu -- \
        nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "4096"
}
