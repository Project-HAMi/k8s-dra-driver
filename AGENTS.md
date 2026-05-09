# Agent Specification & Development Guidelines

This document outlines the operational principles for AI-assisted development and defines the software agents (components) within the `k8s-dra-driver` project. This structure supports **Specification-Driven Development (SDD)** by ensuring both the assistant and the system follow rigorous planning and architectural standards.

> **Spec Version:** `5f92650` — This document reflects the codebase as of commit `5f92650`.

---

## Part 1: AI Assistant Operational Framework

These rules govern the interaction between the AI Assistant and the Lead Engineer.

### 1. Core Philosophy
* **User Context:** User is a senior backend/database engineer.
* **Mantra:** "Slow is Fast." Prioritize reasoning quality, abstraction, and long-term maintainability over speed.
* **Goal:** High-quality solutions with minimal iterations.

### 2. Reasoning & Planning (Global Rules)
Before any action, the assistant must perform internal reasoning:
1.  **Constraints First:** Adhere to explicit rules, library versions, and performance requirements.
2.  **Abductive Reasoning:** When troubleshooting, construct 1–3 hypotheses and verify the most likely cause first.
3.  **Risk Assessment:** Identify high-risk operations (API changes, data migration, history rewriting). Provide safer alternatives when possible.
4.  **Information Sufficiency:** Only ask for clarification if missing info significantly impacts the solution.

### 3. Task Modes
*   **Trivial:** Minor fixes (<10 lines). Direct implementation.
*   **Moderate/Complex:** Requires the **Plan -> Code** workflow.
    *   **Plan Mode:** Define goals, analyze root causes, and propose 1–3 alternative architectures with trade-offs.
    *   **Code Mode:** Implement the chosen plan using minimal, reviewable patches.

### 4. Quality Standards
*   **Readability > Correctness > Performance.**
*   Avoid "code smells": duplication, tight coupling, and over-engineering.
*   **Testing:** Non-trivial changes must include test cases and verification steps.

---

## Part 2: System Agent Specification (k8s-dra-driver)

This section defines the architectural agents within the project for SDD.

### 1. HAMi Kubelet Plugin (Driver Agent)
**Source:** `cmd/hami-kubelet-plugin/driver.go`
*   **Role:** Primary interface between Kubernetes Kubelet and the hardware.
*   **Responsibilities:**
    *   Resource registration and discovery via DRA (Dynamic Resource Allocation).
    *   Handling `PrepareResourceClaims` and `UnprepareResourceClaims` gRPC calls (batched kubelet plugin API).
    *   Driver name: `hami-core-gpu.project-hami.io`.
    *   Publishing `ResourceSlice` objects to the API Server via `pluginhelper.PublishResources`.
    *   Selecting between two `ResourceSlice` publishing strategies based on detected Kubernetes server version:
        *   **Split slices** (`generateSplitResourceSlices`): one `SharedCounters`-only slice + one devices-only slice per GPU — required for k8s ≥ 1.35.
        *   **Combined slices** (`generateCombinedResourceSlices`): one slice per GPU containing both `SharedCounters` and `Devices` — required for k8s 1.34.
    *   Detecting the API server version via `shouldUseSplitResourceSlices` to choose the correct strategy at startup (gated by `featuregates.DynamicMIG`).
    *   Monitoring device health events from `nvmlDeviceHealthMonitor` and republishing `ResourceSlice` with only healthy devices.
    *   Managing the `CheckpointCleanupManager` lifecycle (start/stop).
    *   Re-advertising `ResourceSlice` after prepare/unprepare when `featuregates.PassthroughSupport` is enabled.
*   **Constraints:**
    *   Must ensure thread-safe prepare/unprepare using the node-global file-based `pulock` (`pu.lock`, 10 s timeout).
    *   `Shutdown` must drain the `deviceHealthMonitor`, stop the `healthcheck`, and wait for all goroutines before stopping the `pluginhelper`.

### 2. Device State Agent
**Source:** `cmd/hami-kubelet-plugin/device_state.go`
*   **Role:** Source of truth for device allocation and preparation state on the node.
*   **Responsibilities:**
    *   Maintains two views of allocatable devices:
        *   `allocatable` (`AllocatableDevices`) — flat map for O(1) lookup by device name.
        *   `perGPUAllocatable` (`PerGPUMinorAllocatableDevices`) — devices grouped by physical GPU minor number, used for per-GPU `ResourceSlice` generation.
    *   Orchestrates the full prepare/unprepare lifecycle: `Prepare`, `Unprepare`, `prepareDevices`, `unprepareDevices`.
    *   Implements **transactional checkpoint management**:
        *   Two-phase checkpoint states: `PrepareStarted` → `PrepareCompleted`.
        *   Checkpoint reads and writes are serialized under a dedicated file-based lock (`cp.lock`, 10 s timeout) via `updateCheckpoint`.
    *   Performs **overlap validation** (`validateNoOverlappingPreparedDevices`) to prevent double-allocation of the same device across different claims (admin-access allocations are exempt) — **skipped when `HAMiCoreSupport` is enabled** because the HAMi-Core injection model does not allow idempotent re-prepare of completed claims.
    *   **HAMiCore-specific prepare behavior:** When `HAMiCoreSupport` is enabled, claims in `PrepareCompleted` state are rejected on re-prepare (non-idempotent); partial prepare rollback is also skipped.
    *   Manages sub-managers, each activated by a feature gate:
        *   `HAMiCoreManager` — HAMi-Core GPU virtualization (`featuregates.HAMiCoreSupport`).
        *   `TimeSlicingManager` — CUDA time-slicing (`featuregates.TimeSlicingSettings`).
        *   `MpsManager` — NVIDIA MPS daemon lifecycle (`featuregates.MPSSupport`).
        *   `VfioPciManager` — vfio-pci driver binding for GPU passthrough (`featuregates.PassthroughSupport`).
    *   Warms up the CDI device spec cache at startup for all discovered full GPUs.
    *   Implements `DestroyUnknownMIGDevices` — tears down orphaned MIG devices at startup when `featuregates.DynamicMIG` is enabled, using the checkpoint as the authoritative source of truth.
    *   Delegates MIG device creation/deletion to `deviceLib` (`createMigDevice`, `deleteMigDevice`).
    *   Exposes `UpdateDeviceHealthStatus` for the Driver Agent to update per-device health without a full re-enumerate.

### 3. HAMi Core Manager (Enforcement Agent)
**Source:** `cmd/hami-kubelet-plugin/hami_core.go`
*   **Role:** Implements GPU virtualization and soft resource-limit enforcement using the HAMi-Core library.
*   **Responsibilities:**
    *   Wraps a physical `GpuInfo` into a `HAMiGpuInfo` device (`wrapHAMiCoreGpu`), which exposes `cores` and `memory` as partitionable `CapacityRequestPolicy` fields with step/min/max constraints.
    *   Implements `GetCDIContainerEdits` to produce `CDI ContainerEdits` for a prepared claim:
        *   Creates a per-claim cache directory under `<hostHookPath>/vgpu/claims/<claimUID>/`.
        *   Mounts `libvgpu.so` and `ld.so.preload` from `<hostHookPath>/vgpu/` into the container (read-only).
        *   Mounts the claim cache directory and `/tmp/vgpulock` as read-write bind mounts.
        *   Injects per-device environment variables: `CUDA_DEVICE_SM_LIMIT_<idx>`, `CUDA_DEVICE_MEMORY_LIMIT_<idx>`, and `CUDA_DEVICE_MEMORY_SHARED_CACHE`.
        *   Reads actual consumed capacity (`cores`, `memory`) from `result.ConsumedCapacity` in the claim status.
    *   Implements `Unprepare` — removes the claim cache directory on unprepare.
    *   Defines `HAMiGpuDeviceType` (`hami-gpu`) constant and `PreparedHAMiGpu` type for tracking prepared HAMi-Core allocations.
*   **Constraints:**
    *   `featuregates.HAMiCoreSupport` is mutually exclusive with `TimeSlicingSettings`, `MPSSupport`, `PassthroughSupport`, `DynamicMIG`, and `ComputeDomainCliques`.
    *   `SDD Interface:` Must support mapping arbitrary capacity requests to specific vendor-neutral CDI specifications.

### 4. Hardware Interface Agent (Device Lib)
**Source:** `cmd/hami-kubelet-plugin/nvlib.go`
*   **Role:** Low-level abstraction for NVIDIA hardware and driver libraries.
*   **Responsibilities:**
    *   Constructs a `deviceLib` struct that wraps `nvml.Interface`, `nvdev.Interface`, and `nvpci.Interface`.
    *   Enumerates all possible devices (`enumerateAllPossibleDevices`), returning both the flat `AllocatableDevices` map and the `PerGPUMinorAllocatableDevices` grouped map.
    *   Extracts hardware metadata: UUIDs, minor numbers, PCIe Bus IDs, memory size, driver versions, MIG profiles, addressing mode.
    *   Manages a **long-lived NVML session** when `featuregates.DynamicMIG` is enabled (initialized in `newDeviceLib`, shut down via `alwaysShutdown`).
    *   Provides MIG device lifecycle operations: `createMigDevice`, `deleteMigDevice`, `FindMigDevBySpec`, `obliterateStaleMIGDevices`.
    *   Discovers VFIO devices (`discoverVfioDevice`) and GPU-by-PCIe-bus-ID (`discoverGPUByPCIBusID`) for the passthrough flow.
    *   Wraps physical GPUs as HAMi-Core devices via `wrapHAMiCoreGpu` (defined in `hami_core.go`).
    *   Maintains internal lookup maps for hot-path operations: `gpuInfosByUUID`, `gpuUUIDbyMinor`, `devhandleByUUID`.

### 5. VFIO-PCI Manager (Passthrough Agent)
**Source:** `cmd/hami-kubelet-plugin/vfio-device.go`
*   **Role:** Manages GPU passthrough by binding/unbinding GPUs between the `nvidia` and `vfio-pci` kernel drivers.
*   **Responsibilities:**
    *   Validates passthrough prerequisites at startup: checks that the `vfio_pci` kernel module is loaded and IOMMU is enabled (`ValidatePassthroughSupport`).
    *   `Configure` — moves a GPU from the `nvidia` driver to `vfio-pci` by: waiting for GPU to be free, verifying no SR-IOV VFs are active, then calling the `unbind_from_driver.sh` / `bind_to_driver.sh` scripts.
    *   `Unconfigure` — rebinds the GPU back to the `nvidia` driver.
    *   Generates CDI `ContainerEdits` that expose the VFIO device node (`/dev/vfio/<iommu_group>`) and suppress `NVIDIA_VISIBLE_DEVICES` via `GetVfioCDIContainerEdits` / `GetVfioCommonCDIContainerEdits`.
    *   Uses per-PCIe-Bus-ID mutex (`perGpuLock`) to ensure concurrent configure/unconfigure operations for the same GPU do not interleave.
*   **Constraints:** Activated only when `featuregates.PassthroughSupport` is enabled. Mutually exclusive with `DynamicMIG`, `MPSSupport`, and `HAMiCoreSupport`.

### 6. Checkpoint Cleanup Manager
**Source:** `cmd/hami-kubelet-plugin/cleanup.go`
*   **Role:** Background reconciler that garbage-collects stale checkpoint entries for claims that no longer exist in the Kubernetes API.
*   **Responsibilities:**
    *   Runs a periodic cleanup loop (interval: `ResourceClaimCleanupInterval` = 10 min) that cross-references checkpoint entries against the live Kubernetes `ResourceClaim` API.
    *   For each stale checkpoint entry (claim not found in the API), invokes the driver's `nodeUnprepareResource` callback to perform a full, ordered unprepare.
    *   Exposes a `queue` channel so that other code paths can trigger an out-of-band cleanup immediately (e.g., after an `Unprepare` call).
*   **Constraints:** Holds a reference to `DeviceState` and the DRA `draclient.Client`. Must be started after the checkpoint is initialized and stopped cleanly before process exit.

### 7. Device Health Monitor
**Source:** `cmd/hami-kubelet-plugin/device_health.go`
*   **Role:** Detects GPU hardware faults in real time using NVML XID events.
*   **Responsibilities:**
    *   Subscribes to the NVML event set for all allocatable GPUs and MIG devices.
    *   Maps incoming NVML events to `AllocatableDevice` objects via a `devicePlacementMap` (keyed by `<parentUUID, GI, CI>`).
    *   Publishes unhealthy devices to the `Unhealthy() <-chan *AllocatableDevice` channel consumed by the Driver Agent.
    *   Filters known benign XIDs via a configurable skip-list (`additionalXidsToIgnore` flag).
*   **Constraints:** Activated only when `featuregates.NVMLDeviceHealthCheck` is enabled. Mutually exclusive with `DynamicMIG`.

### 8. MIG Management (Dynamic MIG)
**Source:** `cmd/hami-kubelet-plugin/mig.go`
*   **Role:** Provides data types and utilities for Dynamic MIG (Multi-Instance GPU) device lifecycle management.
*   **Responsibilities:**
    *   Defines `MigSpecTuple` — an abstract 3-tuple `(ParentMinor, ProfileID, PlacementStart)` that precisely identifies a desired MIG configuration without requiring a live device or UUID.
    *   Defines `MigLiveTuple` — the concrete counterpart, adding `MigUUID` and `ParentUUID` once the MIG device has been created.
    *   Provides `NewMigSpecTupleFromCanonicalName` for parsing canonical device names (e.g., `gpu-1-mig-2g47gb-14-0`) back into `MigSpecTuple`.
*   **Constraints:** Used exclusively within the `DynamicMIG` feature gate code paths.

---

## Part 3: Feature Gate Registry
**Source:** `pkg/featuregates/featuregates.go`

Feature gates are controlled at runtime via the `--feature-gates` CLI flag. They use the standard Kubernetes `featuregate.MutableVersionedFeatureGate` mechanism.

> **Implementation note:** The global feature gate registry is exposed through a lazily-initialized singleton accessor `FeatureGates()` (thread-safe via `sync.Once`). Code must call `featuregates.Enabled(...)` or `featuregates.FeatureGates()` rather than accessing a top-level variable directly. The `ValidateFeatureGates()` function enforces all mutual-exclusion and dependency rules at process startup.

| Feature Gate              | Stage | Default | Description |
|---------------------------|-------|---------|-------------|
| `HAMiCoreSupport`         | Alpha | **true** | Mount HAMi-Core `libvgpu.so` into containers; enables soft GPU core/memory limits. |
| `TimeSlicingSettings`     | Alpha | false | Allow per-claim CUDA time-slicing configuration. |
| `MPSSupport`              | Alpha | false | Lifecycle management for NVIDIA MPS control daemons. |
| `IMEXDaemonsWithDNSNames` | Beta  | true  | Use DNS names instead of raw IPs for IMEX daemons. |
| `PassthroughSupport`      | Alpha | false | Bind/unbind GPUs to `vfio-pci` for VM/container passthrough. |
| `DynamicMIG`              | Alpha | false | Dynamic on-demand MIG device creation and teardown. |
| `NVMLDeviceHealthCheck`   | Alpha | false | Real-time GPU health monitoring via NVML XID events. |
| `ComputeDomainCliques`    | Beta  | false | Use `ComputeDomainClique` CRDs for NVLink fabric topology. |
| `CrashOnNVLinkFabricErrors` | Beta | true | Crash instead of fallback on NVLink fabric initialization errors. |

**Mutual exclusion rules enforced at startup (`ValidateFeatureGates`):**
*   `HAMiCoreSupport` ⊕ `TimeSlicingSettings`, `MPSSupport`, `PassthroughSupport`, `DynamicMIG`, `ComputeDomainCliques`
*   `DynamicMIG` ⊕ `PassthroughSupport`, `NVMLDeviceHealthCheck`, `MPSSupport`
*   `ComputeDomainCliques` requires `IMEXDaemonsWithDNSNames`

---

## Part 4: Interaction & Workflow Standards

### SDD Development Cycle
1.  **Specification:** Update this document if a new component (Agent) is introduced or an interface changes.
2.  **Plan:** Describe how agents will interact to fulfill a feature (e.g., how `Driver` calls `HAMiCoreManager` to inject resource limits into CDI edits).
3.  **Implement:** Code changes must match the agent's defined scope in Part 2.
4.  **Verify:** Use `demo/yaml/` for end-to-end validation of agent coordination.

### Commit Guidelines
*   Messages should be concise and explain the "why".
*   Use `gh` CLI for PR management.
*   Never use `git push --force` or history-rewriting commands unless explicitly requested.

---

## Part 5: Build & Deployment Specification

This section details how the agents are packaged and delivered.

### 1. Project Versioning
**Source:** `versions.mk`

| Variable | Value | Purpose |
|---|---|---|
| `VERSION` | `v0.1.0` | HAMi DRA driver release version. |
| `NVVERSION` | `25.12.0` | Upstream `k8s-dra-driver-gpu` version this project is based on. |
| `REGISTRY` | `projecthami` | Default container registry. |
| `GOLANG_VERSION` | (from `hack/golang-version.sh`) | Go compiler version for the build stage. |

### 2. Container Image Structure
The project produces a single distroless-based container image that bundles all necessary agents and libraries.

**Source:** `deploy/container/Dockerfile`

*   **Base Image:** `nvcr.io/nvidia/distroless/cc:v4.0.1-dev` (Minimalist, secure).
*   **Build Stages:**
    1.  `hami-core-build`: Uses `nvidia/cuda:12.3.2-devel-ubuntu20.04` to compile `libvgpu.so` from `lib/hami-core/` via `build.sh`. Also stages `ld.so.preload` and `vgpu-init.sh`.
    2.  `build`: Uses `nvcr.io/nvidia/base/ubuntu:jammy-20251013` to compile the `hami-kubelet-plugin` Go binary via `make cmds`. Supports cross-compilation for `amd64`/`arm64`.
    3.  `bash`: Pulls a pre-built static `bash` binary from `ghcr.io/nvidia/k8s-dra-driver-gpu:v25.12.0-dev-839e966a` (avoids slow/fragile cross-compiled bash builds).
    4.  `toolkit`: Extracts `nvidia-cdi-hook` from `${TOOLKIT_CONTAINER_IMAGE}`.
    5.  `final` (distroless): Assembles artifacts from all prior stages.

### 3. Key Artifacts in Image

| Path in Image | Source Stage | Purpose |
|---|---|---|
| `/usr/bin/hami-kubelet-plugin` | `build` | Main Driver Agent binary. |
| `/usr/local/lib/hami/libvgpu.so` | `hami-core-build` | Enforcement library injected into containers. |
| `/usr/local/lib/hami/ld.so.preload` | `hami-core-build` | Preload config that activates `libvgpu.so` in containers. |
| `/usr/bin/vgpu-init.sh` | `hami-core-build` | Node-level initialization script for vGPU. |
| `/usr/bin/nvidia-cdi-hook` | `toolkit` | Helper hook for NVIDIA CDI device setup. |
| `/usr/bin/kubelet-plugin-prestart.sh` | host (`hack/`) | Pre-start script executed before the plugin binary. |
| `/usr/bin/bind_to_driver.sh` | `build` (from `scripts/`) | Binds a PCI device to a target kernel driver (used by `VfioPciManager`). |
| `/usr/bin/unbind_from_driver.sh` | `build` (from `scripts/`) | Unbinds a PCI device from its current kernel driver (used by `VfioPciManager`). |
| `/bin/bash` | `bash` | Statically linked bash for executing shell scripts in the distroless environment. |

### Helm Chart Deployment
The project also provides a Helm chart under `chart/hami-dra-driver/` for cluster installation.

```bash
# Install via Helm (example)
helm install hami-dra-driver ./chart/hami-dra-driver \
  --namespace hami-system --create-namespace \
  --set image.repository=projecthami/hami-kubelet-plugin \
  --set image.tag=v0.1.0
```

Key templates:
- `daemonset.yaml` — Deploys the kubelet plugin DaemonSet.
- `rbac-kubeletplugin.yaml.yaml` — RBAC including granular DRA status authorization rules.
- `deviceclass-hami-gpu.yaml` — The `DeviceClass` for `hami-core-gpu.project-hami.io`.
- `validation.yaml` — Helm validation hooks.

### 4. Build Commands
The build is orchestrated via `Makefile` (top-level) and `deploy/container/Makefile` (image builds).

```bash
# Build Go binaries only (local)
make cmds

# Build and run tests
make test

# Build the container image (single-platform, local arch)
make image
# or equivalently:
make -f deploy/container/Makefile build

# Multi-arch build (amd64 + arm64), push to registry
make -f deploy/container/Makefile build BUILD_MULTI_ARCH_IMAGES=true PUSH_ON_BUILD=true
```

**Key Build Arguments (passed to `docker buildx`):**

| Argument | Source | Purpose |
|---|---|---|
| `GOLANG_VERSION` | `hack/golang-version.sh` | Go compiler version. |
| `TOOLKIT_CONTAINER_IMAGE` | `hack/toolkit-container-image.sh` | Source image for `nvidia-cdi-hook`. |
| `HAMI_CORE_BUILD_IMAGE` | `versions.mk` | CUDA image used to compile `libvgpu.so`. |
| `VERSION` | `versions.mk` | Embedded in the binary at link time. |
| `GIT_COMMIT` | `git describe` | Embedded in the binary at link time. |

---

## Part 6: Recent Updates

| Commit | Message | Key Impact |
|--------|---------|------------|
| `5f92650` | fix: missing granular DRA status authorization RBAC rules | Helm chart RBAC now includes granular DRA status authorization. |
| `d6aa6b6` | feat: upgrade hami-core for vLLM issues | Bumped `lib/hami-core` submodule to fix vLLM compatibility issues. |
| `0d0d90a` | feat: Support install with helm chart | Added `chart/hami-dra-driver/` for Helm-based cluster deployment. |
| `a2ad09e` | fix: inject failed for hami-gpu | Prepare logic bypasses overlap validation and partial-rollback when `HAMiCoreSupport` is enabled; completed claims are non-idempotent. |
| `6841f23` | fix: invalide featuregates | `pkg/flags/` package extracted for reusable CLI flags (`FeatureGateConfig`, `LoggingConfig`, `KubeClientConfig`); `ComputeDomainCliques` default changed to `false`. |
