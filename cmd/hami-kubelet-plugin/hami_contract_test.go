/*
Copyright 2026 The HAMi Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

// Contract tests for the HAMi-Core prepare path. They drive DeviceState.Prepare
// and Unprepare and assert on the CDI spec written to disk, which is what the
// container runtime consumes. NVML is never touched: the CDI handler's spec
// cache is pre-seeded, so these run in CI without a GPU.
//
// Assertions deliberately avoid the per-claim cache directory layout and the
// CUDA_DEVICE_MEMORY_SHARED_CACHE value, which are implementation details of
// HAMiCoreManager rather than part of the container-facing contract.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/Project-HAMi/k8s-dra-driver/pkg/featuregates"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/version"
)

const (
	testHostHookPath = "/host/hook"
	testNodeName     = "node-0"
	testGPUMemory    = 16 * 1024 * 1024 * 1024
)

func testHAMiGpu() *AllocatableDevice {
	return &AllocatableDevice{
		HAMiGpu: &HAMiGpuInfo{
			GpuInfo: GpuInfo{
				UUID:                  "GPU-test-0",
				memoryBytes:           testGPUMemory,
				cudaComputeCapability: "8.0",
				driverVersion:         "550.54.15",
				cudaDriverVersion:     "12.4",
				pcieRootAttr: &deviceattribute.DeviceAttribute{
					Name:  deviceattribute.StandardDeviceAttributePCIeRoot,
					Value: resourceapi.DeviceAttribute{StringValue: ptr.To("pci0000:00")},
				},
				health: Healthy,
			},
		},
	}
}

// newTestPrepareDeviceState returns a DeviceState with a single HAMi-Core GPU
// that can run the full Prepare/Unprepare flow against temp directories.
func newTestPrepareDeviceState(t *testing.T) *DeviceState {
	t.Helper()
	// Prepare reads feature gates here and in the upstream driver, both of
	// which parse a version that is only injected via -ldflags.
	if version.NVVesion() == "" {
		t.Skip("requires the version injected by -ldflags; run `make test`")
	}
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	gpu := testHAMiGpu()

	cdi := &CDIHandler{
		cdiRoot:   t.TempDir(),
		specCache: utilcache.NewExpiring(),
	}
	cdi.specCache.Set("commonEdits", &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{Env: []string{"COMMON=1"}},
	}, time.Hour)
	cdi.specCache.Set(gpu.UUID(), []cdispec.Device{{
		ContainerEdits: cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/nvidia0"}},
		},
	}}, time.Hour)

	s := newTestDeviceState(t)
	s.cdi = cdi
	s.hamiCoreManager = &HAMiCoreManager{hostHookPath: testHostHookPath}
	s.allocatable = AllocatableDevices{gpu.CanonicalName(): gpu}
	require.NoError(t, s.createCheckpoint(context.Background(), &Checkpoint{V2: &CheckpointV2{NodeBootID: testBootID}}))
	return s
}

// testHAMiClaim returns a claim allocated on hami-gpu-0 with the given capacity.
func testHAMiClaim(t *testing.T, uid string, cores int64, memory string) *resourceapi.ResourceClaim {
	t.Helper()
	// HAMiCoreManager writes the claim cache below the fixed in-container
	// hookPath. Remove what a privileged test run may have left there.
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(hookPath, "vgpu", "claims", uid)) })

	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim-" + uid, UID: types.UID(uid)},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{{
					Request: "gpu",
					Driver:  DriverName,
					Pool:    testNodeName,
					Device:  "hami-gpu-0",
					ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
						"cores":  *resource.NewQuantity(cores, resource.DecimalSI),
						"memory": resource.MustParse(memory),
					},
				}}},
			},
		},
	}
}

func claimSpecPath(s *DeviceState, claimUID string) string {
	return filepath.Join(s.cdi.cdiRoot, cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)+".yaml")
}

// claimDeviceEdits returns the container edits of the claim's only CDI device.
func claimDeviceEdits(t *testing.T, s *DeviceState, claimUID string) cdispec.ContainerEdits {
	t.Helper()
	spec, err := cdiapi.ReadSpec(claimSpecPath(s, claimUID), 0)
	require.NoError(t, err)
	require.Len(t, spec.Devices, 1)
	require.Equal(t, claimUID+"-hami-gpu-0", spec.Devices[0].Name)
	return spec.Devices[0].ContainerEdits
}

func TestHAMiGpuInfoGetDevice(t *testing.T) {
	device := testHAMiGpu().GetDevice()

	require.Equal(t, "hami-gpu-0", device.Name)
	require.Equal(t, HAMiGpuDeviceType, *device.Attributes["type"].StringValue)
	require.True(t, *device.AllowMultipleAllocations, "HAMi-Core GPUs must be shareable across claims")

	cores := device.Capacity["cores"]
	require.Equal(t, int64(100), cores.Value.Value())
	require.Equal(t, int64(100), cores.RequestPolicy.Default.Value())
	require.Equal(t, int64(0), cores.RequestPolicy.ValidRange.Min.Value())
	require.Equal(t, int64(100), cores.RequestPolicy.ValidRange.Max.Value())
	require.Equal(t, int64(1), cores.RequestPolicy.ValidRange.Step.Value())

	memory := device.Capacity["memory"]
	require.Equal(t, int64(testGPUMemory), memory.Value.Value())
	require.Equal(t, int64(testGPUMemory), memory.RequestPolicy.Default.Value())
	require.Equal(t, int64(1<<20), memory.RequestPolicy.ValidRange.Min.Value())
	require.Equal(t, int64(testGPUMemory), memory.RequestPolicy.ValidRange.Max.Value())
	require.Equal(t, int64(1<<20), memory.RequestPolicy.ValidRange.Step.Value())
}

func TestWrapHAMiCoreGpu(t *testing.T) {
	gpu := testHAMiGpu().HAMiGpu.GpuInfo

	wrapped := deviceLib{}.wrapHAMiCoreGpu(&AllocatableDevice{Gpu: &gpu})

	require.Nil(t, wrapped.Gpu, "the full-GPU view must not be announced alongside the HAMi-Core device")
	require.Equal(t, HAMiGpuDeviceType, wrapped.Type())
}

func TestHAMiPrepareWritesCDISpec(t *testing.T) {
	ctx := context.Background()
	s := newTestPrepareDeviceState(t)

	devices, err := s.Prepare(ctx, testHAMiClaim(t, "uid-a", 30, "4Gi"))
	require.NoError(t, err)

	require.Equal(t, []kubeletplugin.Device{{
		Requests:     []string{"gpu"},
		PoolName:     testNodeName,
		DeviceName:   "hami-gpu-0",
		CDIDeviceIDs: []string{cdiVendor + "/" + cdiClaimClass + "=uid-a-hami-gpu-0"},
	}}, devices)

	edits := claimDeviceEdits(t, s, "uid-a")
	require.Equal(t, "/dev/nvidia0", edits.DeviceNodes[0].Path, "the GPU device spec must be kept")
	require.Contains(t, edits.Env, "CUDA_DEVICE_SM_LIMIT_0=30")
	require.Contains(t, edits.Env, "CUDA_DEVICE_MEMORY_LIMIT_0=4096m")
	require.True(t, slices.ContainsFunc(edits.Env, func(e string) bool {
		return strings.HasPrefix(e, "CUDA_DEVICE_MEMORY_SHARED_CACHE=")
	}))

	mounts := map[string]cdispec.Mount{}
	for _, m := range edits.Mounts {
		mounts[m.ContainerPath] = *m
	}
	require.Equal(t, cdispec.Mount{
		ContainerPath: "/usr/local/vgpu/libvgpu.so",
		HostPath:      testHostHookPath + "/vgpu/libvgpu.so",
		Options:       []string{"ro", "nosuid", "nodev", "bind"},
	}, mounts["/usr/local/vgpu/libvgpu.so"])
	require.Equal(t, cdispec.Mount{
		ContainerPath: "/etc/ld.so.preload",
		HostPath:      testHostHookPath + "/vgpu/ld.so.preload",
		Options:       []string{"ro", "nosuid", "nodev", "bind"},
	}, mounts["/etc/ld.so.preload"])
	require.Equal(t, cdispec.Mount{
		ContainerPath: "/tmp/vgpulock",
		HostPath:      "/tmp/vgpulock",
		Options:       []string{"rw", "nosuid", "nodev", "bind"},
	}, mounts["/tmp/vgpulock"])

	cp, err := s.getCheckpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, ClaimCheckpointStatePrepareCompleted, cp.V2.PreparedClaims["uid-a"].CheckpointState)
}

func TestHAMiPrepareSharesDeviceAcrossClaims(t *testing.T) {
	ctx := context.Background()
	s := newTestPrepareDeviceState(t)

	_, err := s.Prepare(ctx, testHAMiClaim(t, "uid-a", 30, "4Gi"))
	require.NoError(t, err)
	_, err = s.Prepare(ctx, testHAMiClaim(t, "uid-b", 50, "2Gi"))
	require.NoError(t, err, "a HAMi-Core GPU already prepared for one claim must be preparable for another")

	edits := claimDeviceEdits(t, s, "uid-b")
	require.Contains(t, edits.Env, "CUDA_DEVICE_SM_LIMIT_0=50")
	require.Contains(t, edits.Env, "CUDA_DEVICE_MEMORY_LIMIT_0=2048m")
}

// A plugin crash between the two checkpoint writes leaves the claim in
// PrepareStarted. The kubelet's retry must complete it.
func TestHAMiPrepareResumesStartedClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestPrepareDeviceState(t)
	claim := testHAMiClaim(t, "uid-a", 30, "4Gi")
	require.NoError(t, s.updateCheckpoint(ctx, func(cp *Checkpoint) {
		cp.V2.PreparedClaims["uid-a"] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Status:          claim.Status,
			Name:            claim.Name,
			Namespace:       claim.Namespace,
		}
	}))

	_, err := s.Prepare(ctx, claim)
	require.NoError(t, err)

	cp, err := s.getCheckpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, ClaimCheckpointStatePrepareCompleted, cp.V2.PreparedClaims["uid-a"].CheckpointState)
	require.FileExists(t, claimSpecPath(s, "uid-a"))
}

func TestHAMiUnprepareRemovesClaimState(t *testing.T) {
	ctx := context.Background()
	s := newTestPrepareDeviceState(t)
	claim := testHAMiClaim(t, "uid-a", 30, "4Gi")
	_, err := s.Prepare(ctx, claim)
	require.NoError(t, err)

	ref := kubeletplugin.NamespacedObject{
		UID:            claim.UID,
		NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name},
	}
	require.NoError(t, s.Unprepare(ctx, ref))

	require.NoFileExists(t, claimSpecPath(s, "uid-a"))
	cp, err := s.getCheckpoint(ctx)
	require.NoError(t, err)
	require.NotContains(t, cp.V2.PreparedClaims, "uid-a")

	// The kubelet may repeat Unprepare.
	require.NoError(t, s.Unprepare(ctx, ref))
}
