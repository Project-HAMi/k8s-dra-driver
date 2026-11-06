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
    kubectl delete pods,resourceclaims,resourceclaimtemplates -l env=batssuite --ignore-not-found --timeout=60s
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
    local uuid
    uuid=$(claim_gpu_uuids single-gpu)
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


# bats test_tags=fastfeedback
@test "HAMi: 1 pod, 2 GPUs, each with its own limit" {
    local _specpath="tests/bats/specs/two-gpus.yaml"
    local _podname="pod-two-gpus"

    kubectl apply -f "${_specpath}"
    kubectl wait --for=condition=READY pods "${_podname}" --timeout=180s

    local uuids
    uuids=$(claim_gpu_uuids two-gpus | sort -u)
    assert_equal "$(grep -c . <<<"${uuids}")" 2

    # The driver numbers the limit env vars in no fixed order, so only the
    # limits as a set are compared, not which GPU got which.
    run kubectl exec "${_podname}" -- env
    assert_success
    assert_equal "$(sed -n 's/^CUDA_DEVICE_SM_LIMIT_[0-9]*=//p' <<<"${output}" | sort -n | xargs)" "30 60"
    assert_equal "$(sed -n 's/^CUDA_DEVICE_MEMORY_LIMIT_[0-9]*=//p' <<<"${output}" | sort -n | xargs)" "4096m 8192m"

    # Both allocated GPUs are visible, each capped at one of the limits.
    run --separate-stderr kubectl exec "${_podname}" -- \
        nvidia-smi --query-gpu=uuid --format=csv,noheader
    assert_success
    assert_equal "$(sort <<<"${output}")" "${uuids}"
    run --separate-stderr kubectl exec "${_podname}" -- \
        nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits
    assert_success
    assert_equal "$(sort -n <<<"${output}" | xargs)" "4096 8192"
}


# bats test_tags=fastfeedback
@test "HAMi: 2 pods share 1 GPU, each with its own limit" {
    kubectl apply -f tests/bats/specs/shared-gpu.yaml
    kubectl wait --for=condition=READY pods pod-shared-a pod-shared-b --timeout=180s

    local uuid
    uuid=$(claim_gpu_uuids shared-a)
    assert [ -n "${uuid}" ]
    assert_equal "$(claim_gpu_uuids shared-b)" "${uuid}"

    run --separate-stderr kubectl exec pod-shared-a -- \
        nvidia-smi --query-gpu=uuid,memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "${uuid}, 4096"
    run --separate-stderr kubectl exec pod-shared-b -- \
        nvidia-smi --query-gpu=uuid,memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "${uuid}, 12288"
}


# bats test_tags=fastfeedback
@test "HAMi: a pod that does not fit on a full GPU stays Pending" {
    kubectl apply -f tests/bats/specs/shared-gpu.yaml
    kubectl wait --for=condition=READY pods pod-shared-a pod-shared-b --timeout=180s

    kubectl apply -f tests/bats/specs/gpu0-extra-pod.yaml
    kubectl wait --for=condition=PodScheduled=false pods pod-gpu0-extra --timeout=120s
    run kubectl get resourceclaim gpu0-extra -o jsonpath='{.status.allocation}'
    assert_success
    assert_output ""

    # It was the full GPU that kept it Pending: once a pod frees its share,
    # it fits.
    kubectl delete pods pod-shared-a --timeout=60s
    kubectl wait --for=condition=READY pods pod-gpu0-extra --timeout=180s
}


@test "HAMi: a running pod is not affected when the kubelet plugin restarts" {
    local _podname="pod-single-gpu"

    kubectl apply -f tests/bats/specs/single-pod.yaml
    kubectl wait --for=condition=READY pods "${_podname}" --timeout=180s
    local pod_uid
    pod_uid=$(kubectl get pods "${_podname}" -o jsonpath='{.metadata.uid}')

    kubectl rollout restart -n "${DRIVER_NAMESPACE}" daemonset/hami-dra-driver-kubelet-plugin
    wait_for_kubelet_plugin

    # The same pod, its container not restarted, and still limited.
    run kubectl get pods "${_podname}" \
        -o jsonpath='{.metadata.uid} {.status.phase} {.status.containerStatuses[0].restartCount}'
    assert_output "${pod_uid} Running 0"
    run --separate-stderr kubectl exec "${_podname}" -- \
        nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits
    assert_success
    assert_output "4096"
}


@test "HAMi: deleting a pod leaves nothing behind on the node" {
    local _podname="pod-template-gpu"

    kubectl apply -f tests/bats/specs/template-pod.yaml
    kubectl wait --for=condition=READY pods "${_podname}" --timeout=180s
    local pod_uid claim claim_uid
    pod_uid=$(kubectl get pods "${_podname}" -o jsonpath='{.metadata.uid}')
    claim=$(kubectl get pods "${_podname}" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}')
    claim_uid=$(kubectl get resourceclaim "${claim}" -o jsonpath='{.metadata.uid}')

    # All of it is there while the pod runs, so the check below looks in the
    # right places.
    run node_claim_state "${claim_uid}" "${pod_uid}"
    assert_line "/usr/local/vgpu/containers/${pod_uid}_ctr"
    assert_line --regexp "^/var/run/cdi/.*${claim_uid}"
    assert_line --partial "checkpoint.json"

    kubectl delete pods "${_podname}" --timeout=60s
    # The claim generated from the template goes with the pod.
    kubectl wait --for=delete resourceclaim "${claim}" --timeout=60s

    # Unprepare can finish after the claim is gone.
    local deadline=$((SECONDS + 60))
    until [[ -z "$(node_claim_state "${claim_uid}" "${pod_uid}")" ]] || (( SECONDS >= deadline )); do
        sleep 2
    done
    run node_claim_state "${claim_uid}" "${pod_uid}"
    assert_output ""
}
