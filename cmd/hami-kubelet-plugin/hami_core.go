/*
Copyright 2025 The HAMi Authors.

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

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/Masterminds/semver"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)


// For deviceinfo.goh
// TODO: Implements a String method for HAMIGpuInfo
type HAMiGpuInfo struct {
	GpuInfo
}


func (d *HAMiGpuInfo) CanonicalName() string {
	// return fmt.Sprintf("hami-gpu-%d-%d", d.minor, d.hamiIndex)
	return fmt.Sprintf("hami-gpu-%d", d.minor)
}

func (d *HAMiGpuInfo) GetDevice() resourceapi.Device {
	allowed := true
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(string(HAMiGpuDeviceType)),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"minor": {
				IntValue: ptr.To(int64(d.minor)),
			},
			"productName": {
				StringValue: &d.productName,
			},
			"brand": {
				StringValue: &d.brand,
			},
			"architecture": {
				StringValue: &d.architecture,
			},
			"cudaComputeCapability": {
				VersionValue: ptr.To(semver.MustParse(d.cudaComputeCapability).String()),
			},
			"driverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.driverVersion).String()),
			},
			"cudaDriverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.cudaDriverVersion).String()),
			},
			"pcieBusID": {
				StringValue: &d.pcieBusID,
			},
			d.pcieRootAttr.Name: d.pcieRootAttr.Value,
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"cores": {
				Value: *resource.NewQuantity(int64(100), resource.DecimalSI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(100), resource.DecimalSI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min: resource.NewQuantity(int64(0), resource.DecimalSI),
						Max: resource.NewQuantity(int64(100), resource.DecimalSI),
						Step: resource.NewQuantity(int64(1), resource.DecimalSI),
					},
				},
			},
			"memory": {
				Value: *resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min: resource.NewQuantity(int64(1048576), resource.BinarySI),
						Max: resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
						Step: resource.NewQuantity(int64(1048576), resource.BinarySI),
					},
				},
			},
		},
		AllowMultipleAllocations: &allowed,
	}
	return device
}


// For nvlib.go
func (l deviceLib) wrapHAMiCoreGpu(parentDev *AllocatableDevice) *AllocatableDevice {
	hamiGpuInfo := &HAMiGpuInfo{
		GpuInfo: GpuInfo{
			UUID:                  parentDev.Gpu.UUID,
			minor:                 parentDev.Gpu.minor,
			migEnabled:            parentDev.Gpu.migEnabled,
			memoryBytes:           parentDev.Gpu.memoryBytes,
			productName:           parentDev.Gpu.productName,
			brand:                 parentDev.Gpu.brand,
			architecture:          parentDev.Gpu.architecture,
			cudaComputeCapability: parentDev.Gpu.cudaComputeCapability,
			driverVersion:         parentDev.Gpu.driverVersion,
			cudaDriverVersion:     parentDev.Gpu.cudaDriverVersion,
			pcieBusID:             parentDev.Gpu.pcieBusID,
			pcieRootAttr:          parentDev.Gpu.pcieRootAttr,
			migProfiles:           parentDev.Gpu.migProfiles,
			addressingMode:        parentDev.Gpu.addressingMode,
			health:                parentDev.Gpu.health,
		},
	}
	parentDev.HAMiGpu = hamiGpuInfo
	parentDev.Gpu = nil
	return parentDev
}

// For prepared.go
type PreparedHAMiGpu struct {
	Info   *HAMiGpuInfo          `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}

func (l PreparedDeviceList) HAMiGpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == HAMiGpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) HAMiGpuUUIDs() []string {
	var uuids []string
	for _, device := range l.HAMiGpus() {
		uuids = append(uuids, device.HAMiGpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) HAMIGpuUUIDs() []string {
	return g.Devices.HAMiGpus().UUIDs()
}

// For sharing.go
type HAMiCoreManager struct {
	hostHookPath string
	nvdevlib     *deviceLib
}

func NewHAMiCoreManager(deviceLib *deviceLib) *HAMiCoreManager {
	return &HAMiCoreManager{
		nvdevlib:     deviceLib,
		hostHookPath: "/usr/local",
	}
}

func (m *HAMiCoreManager) getConsumableCapacityMap(claim *resourceapi.ResourceClaim) map[string]map[resourceapi.QualifiedName]resource.Quantity {
	resMap := map[string]map[resourceapi.QualifiedName]resource.Quantity{}
	for _, result := range claim.Status.Allocation.Devices.Results {
		devName := result.Device
		if _, exists := resMap[devName]; !exists {
			resMap[devName] = map[resourceapi.QualifiedName]resource.Quantity{}
		}
		maps.Copy(resMap[devName], result.ConsumedCapacity)
	}
	return resMap
}

func (m *HAMiCoreManager) GetCDIContainerEdits(claim *resourceapi.ResourceClaim, devs AllocatableDevices) *cdiapi.ContainerEdits {
	cacheFileHostDirectory := fmt.Sprintf("%s/vgpu/claims/%s", m.hostHookPath, claim.UID)
	// TODO: We should check the status of claim, becasue there may be two pod share the claim
	var err error
	err = os.RemoveAll(cacheFileHostDirectory)
	if err != nil {
		klog.Warningf("Failed to remove host directory for cachefile %s: %s", cacheFileHostDirectory, err)
	}
	err = os.MkdirAll(cacheFileHostDirectory, 0777)
	if err != nil {
		klog.Warningf("Failed to create host directory for cachefile %s: %s", cacheFileHostDirectory, err)
	}
	err = os.Chmod(cacheFileHostDirectory, 0777)
	if err != nil {
		klog.Warningf("Failed to change mod of host directory for cachefile %s: %s", cacheFileHostDirectory, err)
	}

	hamiEnvs := []string{}
	// TOOD: Get SM Limit from Claim's Annotation
	hamiEnvs = append(hamiEnvs, fmt.Sprintf("CUDA_DEVICE_MEMORY_SHARED_CACHE=%s", fmt.Sprintf("%s/%v.cache", cacheFileHostDirectory, uuid.New().String())))

	devCapMap := m.getConsumableCapacityMap(claim)
	idx := 0
	for name, dev := range devs {
		// TODO: The idx here may not equals to the index in nvidia-smi, So we need to find a solution to solve it
		klog.Warningf("HAMiCoreManager GetCDIContainerEdits for dev: %s\n", name)
		capNameSMLimit := resourceapi.QualifiedName("cores")
		capNameMemoryLimit := resourceapi.QualifiedName("memory")
		SMLimitEnv := fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_%d=%s", idx, "60")
		memoryLimit := string(strconv.FormatUint(dev.HAMiGpu.memoryBytes/1024/1024, 10)) + "m"
		MemoryLimitEnv := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%d=%s", idx, memoryLimit)
		// TODO: Loop in a map getting from HAMiCoreManager
		if _, ok := devCapMap[name]; ok {
			if _, ok := devCapMap[name][capNameSMLimit]; ok {
				q := devCapMap[name][capNameSMLimit]
				val, succ := q.AsInt64()
				if succ {
					SMLimitEnv = fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_%d=%s", idx, strconv.FormatInt(val, 10))
				}
			}
			if _, ok := devCapMap[name][capNameMemoryLimit]; ok {
				q := devCapMap[name][capNameMemoryLimit]
				val, succ := q.AsInt64()
				if succ {
					MemoryLimitEnv = fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%d=%s", idx, strconv.FormatInt(val/1024/1024, 10)+"m")
				}
			}
		}
		hamiEnvs = append(hamiEnvs, SMLimitEnv, MemoryLimitEnv)
		idx++
	}

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: hamiEnvs,
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: cacheFileHostDirectory,
					HostPath:      cacheFileHostDirectory,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: m.hostHookPath + "/vgpu/libvgpu.so",
					HostPath:      m.hostHookPath + "/vgpu/libvgpu.so",
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				// TODO: Check CUDA_DISABLE_CONTROL env before mount ld.so.preload
				{
					ContainerPath: "/etc/ld.so.preload",
					HostPath:      m.hostHookPath + "/vgpu/ld.so.preload",
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: "/tmp/vgpulock",
					HostPath:      "/tmp/vgpulock",
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}
}

func (m *HAMiCoreManager) Unprepare(claimUID string, pl PreparedDeviceList) error {
	path := fmt.Sprintf("%s/vgpu/claims/%s", m.hostHookPath, claimUID)
	_ = os.RemoveAll(path)
	return nil
}

// For types.go
const HAMiGpuDeviceType = "hami-gpu"
