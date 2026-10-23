/*
 * Copyright 2026 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	nvmlmock "github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/featuregates"
	pkgflags "github.com/Project-HAMi/k8s-dra-driver/pkg/flags"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// Embed the hardware interface so unexpected calls fail, instead of silently
// succeeding. Only discovery calls used by this full-GPU fixture are simulated.
type memoryTestDevice struct {
	nvdev.Device
	index      int
	status     nvml.Return
	total      uint64
	mig        bool
	nameStatus nvml.Return
}

func (d memoryTestDevice) GetMinorNumber() (int, nvml.Return) { return d.index, nvml.SUCCESS }
func (d memoryTestDevice) GetUUID() (string, nvml.Return) {
	return fmt.Sprintf("GPU-test-%d", d.index), nvml.SUCCESS
}
func (d memoryTestDevice) IsMigEnabled() (bool, error) { return d.mig, nil }
func (d memoryTestDevice) GetMemoryInfo() (nvml.Memory, nvml.Return) {
	return nvml.Memory{Total: d.total}, d.status
}
func (d memoryTestDevice) GetName() (string, nvml.Return)                    { return "test GPU", d.nameStatus }
func (d memoryTestDevice) GetArchitectureAsString() (string, error)          { return "Blackwell", nil }
func (d memoryTestDevice) GetBrandAsString() (string, error)                 { return "NVIDIA", nil }
func (d memoryTestDevice) GetCudaComputeCapabilityAsString() (string, error) { return "12.1", nil }
func (d memoryTestDevice) GetPCIBusID() (string, error)                      { return "0000:ff:00.0", nil }
func (d memoryTestDevice) GetAddressingModeAsString() (string, error)        { return "", nil }
func (d memoryTestDevice) GetGpuInstanceProfileInfo(int) (nvml.GpuInstanceProfileInfo, nvml.Return) {
	return nvml.GpuInstanceProfileInfo{}, nvml.ERROR_NOT_SUPPORTED
}

type memoryTestDevices struct {
	nvdev.Interface
	devices []nvdev.Device
}

func (d memoryTestDevices) VisitDevices(visit func(int, nvdev.Device) error) error {
	for i, device := range d.devices {
		if err := visit(i, device); err != nil {
			return err
		}
	}
	return nil
}

func memoryTestLibrary(devices ...nvdev.Device) *deviceLib {
	return &deviceLib{
		Interface: memoryTestDevices{devices: devices},
		nvmllib: &nvmlmock.Interface{
			InitFunc:                       func() nvml.Return { return nvml.SUCCESS },
			ShutdownFunc:                   func() nvml.Return { return nvml.SUCCESS },
			SystemGetDriverVersionFunc:     func() (string, nvml.Return) { return "580.95.05", nvml.SUCCESS },
			SystemGetCudaDriverVersionFunc: func() (int, nvml.Return) { return 13000, nvml.SUCCESS },
		},
		gpuInfosByUUID: make(map[string]*GpuInfo), gpuUUIDbyMinor: make(map[GPUMinor]string), devhandleByUUID: make(map[string]nvml.Device),
	}
}

func TestGPUMemoryDiscovery(t *testing.T) {
	for _, tt := range []struct {
		name                string
		enabled, mig        bool
		status              nvml.Return
		total, budget, want uint64
		wantErr             error
	}{
		{name: "NVML wins", enabled: true, status: nvml.SUCCESS, total: 8589934592, budget: 68719476736, want: 8589934592},
		{name: "successful zero unchanged", enabled: true, status: nvml.SUCCESS, total: 0, budget: 68719476736},
		{name: "configured fallback", enabled: true, status: nvml.ERROR_NOT_SUPPORTED, budget: 68719476736, want: 68719476736},
		{name: "unconfigured remains fatal", enabled: true, status: nvml.ERROR_NOT_SUPPORTED, wantErr: nvml.ERROR_NOT_SUPPORTED},
		{name: "MIG cannot use fallback", enabled: true, status: nvml.ERROR_NOT_SUPPORTED, budget: 68719476736, mig: true, wantErr: errDeviceMemoryUnavailable},
		{name: "disabled preserves unsupported error", status: nvml.ERROR_NOT_SUPPORTED, budget: 68719476736, wantErr: nvml.ERROR_NOT_SUPPORTED},
		{name: "disabled success unchanged", status: nvml.SUCCESS, total: 8589934592, budget: 68719476736, want: 8589934592},
		{name: "GPU lost remains fatal", enabled: true, status: nvml.ERROR_GPU_IS_LOST, budget: 68719476736, wantErr: nvml.ERROR_GPU_IS_LOST},
	} {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, tt.enabled)
			l := memoryTestLibrary()
			l.deviceMemoryBytes = tt.budget
			gpu, err := l.getGpuInfo(0, memoryTestDevice{status: tt.status, total: tt.total, mig: tt.mig})
			require.ErrorIs(t, err, tt.wantErr)
			if tt.wantErr != nil {
				require.Nil(t, gpu)
				if tt.wantErr != errDeviceMemoryUnavailable {
					require.NotErrorIs(t, err, errDeviceMemoryUnavailable)
				}
				return
			}
			require.Equal(t, tt.want, gpu.memoryBytes)
		})
	}
}

func TestGPUMemoryEnumeration(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	l := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED, mig: true}, memoryTestDevice{index: 1, status: nvml.SUCCESS, total: 8589934592})
	l.deviceMemoryBytes = 68719476736
	all, perGPU, err := l.enumerateAllPossibleDevices()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Contains(t, all, "hami-gpu-1")
	require.Len(t, perGPU, 1)
	require.Contains(t, perGPU, 1)
	require.NotContains(t, l.gpuInfosByUUID, "GPU-test-0")
	require.NotContains(t, l.gpuUUIDbyMinor, 0)
	require.Empty(t, l.devhandleByUUID)
	l = memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED, nameStatus: nvml.ERROR_UNKNOWN})
	l.deviceMemoryBytes = 68719476736
	_, _, err = l.enumerateAllPossibleDevices()
	require.Error(t, err)
	require.NotErrorIs(t, err, errDeviceMemoryUnavailable)
}

func TestPublishResourcesUsesNodeMemoryBudget(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node", UID: "node-uid", Annotations: map[string]string{deviceMemoryAnnotation: "1.1Mi"},
	}}
	client := fake.NewClientset(node)
	// The fake API does not implement server-side GenerateName. Supply the
	// generated name before allowing its ordinary object tracker to handle create.
	client.PrependReactor("create", "resourceslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		slice := action.(clienttesting.CreateAction).GetObject().(*resourceapi.ResourceSlice)
		slice.Name = "published-memory-test"
		slice.ResourceVersion = "1"
		return false, nil, nil
	})
	d := &driver{client: client, state: &DeviceState{}}
	helper, err := kubeletplugin.Start(ctx, d,
		kubeletplugin.KubeClient(client), kubeletplugin.DriverName(DriverName),
		kubeletplugin.NodeName(node.Name), kubeletplugin.NodeUID(node.UID),
		kubeletplugin.DRAService(false), kubeletplugin.RegistrationService(false),
	)
	require.NoError(t, err)
	defer helper.Stop()
	d.pluginhelper = helper
	config := &Config{
		flags:      &Flags{nodeName: node.Name, preConfiguredDeviceMemory: "4Gi"},
		clientsets: pkgflags.ClientSets{Core: client},
	}
	discover := func() (AllocatableDevices, error) {
		t.Helper()
		require.NoError(t, config.loadDeviceMemoryBudget(ctx, true))
		lib := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
		lib.deviceMemoryBytes = config.deviceMemoryBytes
		allocatable, _, err := lib.enumerateAllPossibleDevices()
		return allocatable, err
	}
	d.state.allocatable, err = discover()
	require.NoError(t, err)
	require.NoError(t, d.publishResources(ctx, config))
	wantMemory := resource.MustParse("2Mi")
	require.Eventually(t, func() bool {
		slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		if err != nil || len(slices.Items) != 1 || len(slices.Items[0].Spec.Devices) != 1 {
			return false
		}
		device := slices.Items[0].Spec.Devices[0]
		memory := device.Capacity["memory"]
		return device.Name == "hami-gpu-0" && memory.Value.Cmp(wantMemory) == 0
	}, 5*time.Second, 10*time.Millisecond, "GPU was not published with 2Mi of memory")

	// Reload startup configuration with an explicit zero overriding the global default.
	updatedNode := node.DeepCopy()
	updatedNode.Annotations[deviceMemoryAnnotation] = "0"
	_, err = client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = discover()
	require.ErrorIs(t, err, nvml.ERROR_NOT_SUPPORTED)
}
