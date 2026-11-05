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
    iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" --set monitor.enabled=true
}

setup() {
    load 'helpers.sh'
    _common_setup
}

teardown() {
    kubectl delete pods,resourceclaims -l env=batssuite --ignore-not-found --timeout=60s
}

bats::on_failure() {
    show_debug_info
}

# "<pod> <container> <GPU UUID> <limit MiB>" for each vGPU memory limit that
# vGPUmonitor reports for the default namespace, sorted.
vgpu_memory_limits() {
    local pod
    pod=$(kubectl get pods -n "${DRIVER_NAMESPACE}" -l "${KUBELET_PLUGIN_SELECTOR}" -o jsonpath='{.items[0].metadata.name}')
    kubectl get --raw "/api/v1/namespaces/${DRIVER_NAMESPACE}/pods/${pod}:9394/proxy/metrics" | awk '
        /^hami_vgpu_memory_limit_bytes\{/ {
            match($0, /\{.*\}/)
            n = split(substr($0, RSTART + 1, RLENGTH - 2), pairs, ",")
            delete l
            for (i = 1; i <= n; i++) {
                split(pairs[i], kv, "=")
                gsub(/"/, "", kv[2])
                l[kv[1]] = kv[2]
            }
            if (l["namespace"] == "default")
                printf "%s %s %s %d\n", l["pod"], l["container"], l["device_uuid"], $NF / 1048576
        }' | sort
}


# bats test_tags=fastfeedback
@test "HAMi: vGPUmonitor reports the memory limit of each pod" {
    kubectl apply -f tests/bats/specs/shared-gpu.yaml
    kubectl wait --for=condition=READY pods pod-shared-a pod-shared-b --timeout=180s

    # HAMi-Core creates the shared cache that the monitor reads when the
    # workload first uses the GPU.
    kubectl exec pod-shared-a -- nvidia-smi >/dev/null
    kubectl exec pod-shared-b -- nvidia-smi >/dev/null

    local uuid expected
    uuid=$(claim_gpu_uuids shared-a)
    assert [ -n "${uuid}" ]
    expected=$(printf '%s\n' "pod-shared-a ctr ${uuid} 4096" "pod-shared-b ctr ${uuid} 12288")

    local deadline=$((SECONDS + 60))
    until [[ "$(vgpu_memory_limits)" == "${expected}" ]] || (( SECONDS >= deadline )); do
        sleep 5
    done
    run vgpu_memory_limits
    assert_success
    assert_output "${expected}"
}
