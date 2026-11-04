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
    iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}"
}

# Executed before entering each test in this file.
setup() {
    load 'helpers.sh'
    _common_setup
}

# Delete what a test created, also when it failed, so tests stay independent.
teardown() {
    kubectl delete pods,resourceclaims -l env=batssuite --ignore-not-found --timeout=60s
}

bats::on_failure() {
    show_debug_info
}


# bats test_tags=fastfeedback
@test "HAMi: one device per GPU, with its UUID, PCI bus ID, 100 cores and full memory" {
    # "<uuid> <bus:device.function> <memory MiB> <cores>" per GPU, sorted, from
    # both sides. The PCI domain is dropped, because NVML and nvidia-smi print
    # it with different widths.
    local expected published
    expected=$(node_nvidia_smi --query-gpu=uuid,pci.bus_id,memory.total --format=csv,noheader,nounits |
        awk -F', ' '{ n = split(tolower($2), id, ":"); print $1, id[n-1] ":" id[n], $3, 100 }' | sort)
    published=$(hami_devices | jq -r '
        # Capacity is a binary quantity such as "16Gi"; convert it to MiB.
        def mib: capture("^(?<n>[0-9]+)(?<u>[KMGT]i)?$")
            | (.n | tonumber) * {"": 1, Ki: 1024, Mi: 1048576, Gi: 1073741824, Ti: 1099511627776}[.u // ""] / 1048576;
        select(.attributes.type.string == "hami-gpu")
        | (.attributes.pcieBusID.string | ascii_downcase | split(":") | .[-2:] | join(":")) as $bdf
        | "\(.attributes.uuid.string) \($bdf) \(.capacity.memory.value | mib) \(.capacity.cores.value)"' | sort)

    assert [ -n "${expected}" ]
    assert_equal "${published}" "${expected}"

    # Claims and CEL selectors refer to a device as hami-gpu-<minor>, and some
    # select on the minor attribute itself.
    local misnamed
    misnamed=$(hami_devices | jq -r '
        select(.attributes.type.string == "hami-gpu")
        | select(.attributes.minor.int == null or .name != "hami-gpu-\(.attributes.minor.int)")
        | .name')
    assert_equal "${misnamed}" ""
}


# bats test_tags=fastfeedback
@test "HAMi: 1 pod, 1 GPU with 30 cores and 4Gi, limited by HAMi-Core" {
    local _specpath="tests/bats/specs/single-pod.yaml"
    local _podname="pod-single-gpu"

    kubectl apply -f "${_specpath}"
    kubectl wait --for=condition=READY pods "${_podname}" --timeout=180s

    # The claim is allocated one device of this driver.
    local device uuid
    device=$(kubectl get resourceclaim single-gpu -o json |
        jq -r --arg driver "${DRIVER_NAME}" '.status.allocation.devices.results[] | select(.driver == $driver) | .device')
    uuid=$(hami_devices | jq -r --arg name "${device}" 'select(.name == $name) | .attributes.uuid.string')
    assert [ -n "${uuid}" ]

    run kubectl exec "${_podname}" -- env
    assert_line "CUDA_DEVICE_SM_LIMIT_0=30"
    assert_line "CUDA_DEVICE_MEMORY_LIMIT_0=4096m"

    # Only the allocated GPU is visible, capped at the claimed memory.
    run --separate-stderr kubectl exec "${_podname}" -- \
        nvidia-smi --query-gpu=uuid,memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "${uuid}, 4096"
}
