---
name: hami-dra-kind-testing
description: Use when testing the HAMi-Core DRA Driver on a kind cluster — covers cluster setup, Helm-based driver install, ResourceClaim configuration, pod scheduling, HAMi-Core memory limit verification via nvidia-smi, and teardown.
---

# HAMi-Core DRA Driver — kind Cluster Testing

## Overview

This skill guides the complete test cycle of the HAMi-Core DRA Driver on a local kind cluster: from building the image through verifying that Consumable Capacity (GPU core/memory limits) is enforced inside a container.

The **driver** (RBAC + DaemonSet + DeviceClass) is installed via the Helm chart at `chart/hami-dra-driver/`.
The **test workloads** (Namespace, ResourceClaims, ResourceClaimTemplate, Pods) are applied from `demo/yaml/`.

The key end-to-end proof is `nvidia-smi` inside a test pod reporting the **capped** memory (e.g. 4096 MiB) rather than the full physical GPU memory. This works because HAMi-Core's `libvgpu.so` is preloaded into the container and intercepts NVML calls.

---

## Pre-flight Checks

Run this **before** touching the cluster. Every line must return success.

```bash
# 1. NVIDIA driver + CUDA
nvidia-smi

# 2. NVIDIA Container Toolkit
nvidia-ctk --version

# 3. accept-nvidia-visible-devices-as-volume-mounts = true
grep -q "accept-nvidia-visible-devices-as-volume-mounts\s*=\s*true" \
  /etc/nvidia-container-runtime/config.toml && echo "[OK] volume-mounts config"
# Fix: sudo nvidia-ctk config --in-place \
#        --set accept-nvidia-visible-devices-as-volume-mounts=true

# 4. NVIDIA runtime set as default container runtime
docker info 2>/dev/null | grep -i "default runtime" | grep -qi nvidia \
  && echo "[OK] nvidia is default runtime"
# Fix: sudo nvidia-ctk runtime configure --runtime=docker --set-as-default
#      sudo systemctl restart docker

# 5. kind, kubectl, helm
kind version
kubectl version --client
helm version

# 6. Driver image exists locally
docker images --filter reference=projecthami/k8s-dra-driver:v0.1.0 -q | grep -q . \
  && echo "[OK] driver image found"

# 7. Test image exists locally (kind clusters may not have internet)
docker images --filter reference=ubuntu:24.04 -q | grep -q . \
  && echo "[OK] test image found"
# Fix if missing: docker pull ubuntu:24.04
```

> **All checks must pass.** The most common failure is #3 or #4 after a toolkit upgrade.

---

## Key Environment Variables

All variables are sourced from `demo/clusters/kind/scripts/common.sh` and can be overridden by prefixing the script call.

| Variable | Default | Purpose |
|---|---|---|
| `KIND_K8S_TAG` | `v1.34.0` | Kubernetes version (must be ≥ 1.34 for Consumable Capacity) |
| `KIND_CLUSTER_NAME` | `k8s-dra-driver-cluster` | Name of the kind cluster |
| `DRIVER_IMAGE` | `projecthami/k8s-dra-driver:v0.1.0` | Driver image to load into nodes |
| `KIND_CLUSTER_CONFIG_PATH` | `demo/clusters/kind/scripts/kind-cluster-config.yaml` | kind cluster config file |

**Override example:**
```bash
KIND_K8S_TAG=v1.35.0 ./demo/clusters/kind/create-cluster.sh
```

---

## Stage 1 — Build the Driver Image

```bash
# From repo root
make image

# Verify
docker images | grep k8s-dra-driver
# Expected: projecthami/k8s-dra-driver   v0.1.0   ...
```

> Skip this stage if you already have the image pulled from a registry. The cluster creation script will auto-load it.

---

## Stage 2 — Create the kind Cluster

**Check for an existing cluster with the same name first and delete it if present:**

```bash
if kind get clusters | grep -q "^k8s-dra-driver-cluster$"; then
  echo "Existing cluster found — deleting before recreating..."
  ./demo/clusters/kind/delete-cluster.sh
fi
```

**Create the cluster:**
```bash
./demo/clusters/kind/create-cluster.sh
```

This script:
- Creates a kind cluster using `demo/clusters/kind/scripts/kind-cluster-config.yaml`
- Enables required Kubernetes feature gates: `DynamicResourceAllocation`, `DRAConsumableCapacity`, `DRAPartitionableDevices`, `DRAPrioritizedList`, `DRAAdminAccess`, `DRAResourceClaimDeviceStatus`
- Enables CDI in containerd
- Auto-loads `DRIVER_IMAGE` into cluster nodes if the image exists locally

**Pre-load the test workload image** (the worker node usually does **not** have internet access):

```bash
kind load docker-image --name k8s-dra-driver-cluster ubuntu:24.04
```

**Verify:**
```bash
kubectl get nodes
# Expected: control-plane + worker node, both Ready
```

---

## Stage 3 — Install the HAMi DRA Driver (Helm)

> **This skill tests the HAMi-Core feature only.**
> Before installing, ensure `HAMiCoreSupport` is the active feature gate.
> `HAMiCoreSupport` is mutually exclusive with `TimeSlicingSettings`, `MPSSupport`,
> `PassthroughSupport`, and `DynamicMIG` — all of these must be disabled (they are by default).
> When `featureGates` is left empty in `values.yaml`, `HAMiCoreSupport=true` is used
> implicitly because it is the default-enabled gate.

Install from the local chart into the `hami-dra-driver` namespace:

```bash
helm install hami-dra-driver ./chart/hami-dra-driver \
  --namespace hami-dra-driver \
  --create-namespace \
  --set gpuResourcesEnabledOverride=true
```

**What the chart installs:**
- ServiceAccount + ClusterRole + ClusterRoleBinding + Role + RoleBinding (`templates/rbac-kubeletplugin.yaml.yaml`)
- DaemonSet for the kubelet-plugin (`templates/daemonset.yaml`)
- DeviceClass `hami-core-gpu.project-hami.io` (`templates/deviceclass-hami-gpu.yaml`)

**Wait for the driver pod to be ready:**
```bash
kubectl -n hami-dra-driver rollout status daemonset/hami-dra-driver-kubelet-plugin --timeout=120s
```

**Verify ResourceSlices are published (confirms HAMiCoreSupport is active):**
```bash
kubectl get resourceslices -o wide
# Expected: one ResourceSlice per GPU with
#           DRIVER = hami-core-gpu.project-hami.io
```

> If `DRIVER` shows `gpu.nvidia.com` instead, the `HAMiCoreSupport` feature gate is disabled.  
> Check: `kubectl -n hami-dra-driver logs -l app.kubernetes.io/component=kubelet-plugin | grep "Using driver name"`

**Note:** The chart's `validation.yaml` enforces:
- You cannot deploy into the `default` namespace unless `allowDefaultNamespace=true`.
- The `namespace` key in `values.yaml` is deprecated and will fail rendering.
- `gpuResourcesEnabledOverride=true` is required because `resources.gpus.enabled=true` by default.

---

## Stage 4 — Apply Test Workloads

The Helm chart installs the driver and DeviceClass.  
Test workloads (namespace, ResourceClaims, ResourceClaimTemplate) are applied separately:

```bash
kubectl apply -f demo/yaml/setup.yaml
```

This creates:

| Object | Name | Details |
|---|---|---|
| `Namespace` | `test-dra` | Namespace for all test workloads |
| `ResourceClaim` | `single-gpu-0` | 1 device — 30 cores, 4Gi memory |
| `ResourceClaim` | `double-gpu-0` | 2 devices — 30 cores/4Gi + 60 cores/8Gi |
| `ResourceClaimTemplate` | `single-gpu-tpl` | Template for 30 cores, 4Gi memory |

> The `DeviceClass` is already created by the Helm chart. `setup.yaml` also declares it, so applying it is a no-op update. If you prefer to skip it, edit `setup.yaml` and remove the DeviceClass block.

---

## Stage 5 — Create Test Pods and Verify

Three pod manifests are available:

| File | Pod name | Claim | Description |
|---|---|---|---|
| `demo/yaml/pod-0.yaml` | `pod-0` | `single-gpu-0` | Single GPU, pre-created claim |
| `demo/yaml/pod-1.yaml` | `pod-1` | `double-gpu-0` | Two GPUs in one claim |
| `demo/yaml/pod-tpl-0.yaml` | `pod-tpl-1` | `single-gpu-tpl` | Single GPU via ResourceClaimTemplate |

```bash
kubectl create -f demo/yaml/pod-0.yaml
```

**Wait for the pod to become Ready:**
```bash
kubectl -n test-dra wait --for=condition=Ready pod/pod-0 --timeout=120s
```

**Verify HAMi-Core env vars are injected (cores + memory limits):**
```bash
kubectl -n test-dra exec pod-0 -- \
  env | grep -E "CUDA_DEVICE_SM_LIMIT|CUDA_DEVICE_MEMORY_LIMIT|CUDA_DEVICE_MEMORY_SHARED_CACHE"
# Expected:
#   CUDA_DEVICE_SM_LIMIT_0=30
#   CUDA_DEVICE_MEMORY_LIMIT_0=4096m
#   CUDA_DEVICE_MEMORY_SHARED_CACHE=...
```

**Verify memory cap via nvidia-smi (strongest end-to-end proof):**

`libvgpu.so` intercepts NVML calls inside the container, so `nvidia-smi` reports the capped memory — not the full physical GPU memory.

```bash
kubectl -n test-dra exec pod-0 -- \
  nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits
# Expected: 4096
# (matches the 4Gi = 4096 MiB requested in single-gpu-0 ResourceClaim)
```

**Check consumed capacity is recorded in claim status:**
```bash
kubectl -n test-dra get resourceclaim single-gpu-0 \
  -o jsonpath='{.status.allocation}' | python3 -m json.tool 2>/dev/null
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `helm install` fails with "Running in the 'default' namespace is not recommended" | Missing `--namespace` | Add `--namespace hami-dra-driver --create-namespace` |
| `helm install` fails with `gpuResourcesEnabledOverride` guard | `resources.gpus.enabled=true` without override | Add `--set gpuResourcesEnabledOverride=true` |
| Pod stuck `Pending`, event: `no devices available` | Driver pod not Running or ResourceSlice not published | `kubectl -n hami-dra-driver logs -l app.kubernetes.io/component=kubelet-plugin` |
| ResourceSlice `DRIVER` is `gpu.nvidia.com` not `hami-core-gpu.project-hami.io` | `HAMiCoreSupport` feature gate disabled | Check driver logs for `Using driver name:` line; reinstall with `--set featureGates.HAMiCoreSupport=true` |
| Pod status `ImagePullBackOff` for `ubuntu:24.04` | kind worker node can't reach Docker Hub | Pre-load: `kind load docker-image --name k8s-dra-driver-cluster ubuntu:24.04` |
| Pod status `ErrImagePull` / `DeadlineExceeded` | No outbound internet from kind nodes | Ensure both driver image and `ubuntu:24.04` are loaded into kind before creating pods |
| `CUDA_DEVICE_SM_LIMIT` not in pod env | `libvgpu.so` not mounted — init script failed | `kubectl -n hami-dra-driver describe pod <driver-pod>` — check postStart events and hostPath `/usr/local/vgpu` |
| `nvidia-smi` shows full GPU memory (not capped) | `ld.so.preload` not injected or wrong `VGPU_INIT_PATH` | Verify `.Values.driver.vgpuInitPath` mount and `libvgpu.so` exists at that path on the node |
| kind cluster creation fails on `kindest/node` image pull | `KIND_K8S_TAG` image not available locally | Check https://hub.docker.com/r/kindest/node/tags and set a valid tag |
| GPU not visible inside kind worker node | `accept-nvidia-visible-devices-as-volume-mounts` not set | Re-run prerequisite fix #3 and restart docker |

---

## Stage 6 — Cleanup

**Ask the user whether to delete the cluster before proceeding:**

```
The test is complete. Do you want to delete the kind cluster "${KIND_CLUSTER_NAME}"?
  y) Delete cluster (full teardown)
  n) Keep cluster (useful for further debugging)
```

Always clean up the driver and test workloads regardless of the answer:

```bash
# Always: delete test pods and workloads
kubectl delete -f demo/yaml/pod-0.yaml --ignore-not-found

# Always: delete DeviceClass, ResourceClaims, test namespace
kubectl delete -f demo/yaml/setup.yaml --ignore-not-found

# Always: uninstall the driver via Helm
helm uninstall hami-dra-driver --namespace hami-dra-driver

# Optional: delete the namespace if Helm left it behind
kubectl delete namespace hami-dra-driver --ignore-not-found
```

**Only if the user confirms cluster deletion:**

```bash
./demo/clusters/kind/delete-cluster.sh
```
