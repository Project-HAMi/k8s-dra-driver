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
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/Masterminds/semver"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
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
						Min:  resource.NewQuantity(int64(0), resource.DecimalSI),
						Max:  resource.NewQuantity(int64(100), resource.DecimalSI),
						Step: resource.NewQuantity(int64(1), resource.DecimalSI),
					},
				},
			},
			"memory": {
				Value: *resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  resource.NewQuantity(int64(1048576), resource.BinarySI),
						Max:  resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
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
	nodeName     string

	podInformerFactory informers.SharedInformerFactory
	podLister          corelisters.PodLister
	podListerSynced    cache.InformerSynced
	stopCh             chan struct{}
}

func NewHAMiCoreManager(deviceLib *deviceLib, hostHookPath string, clientset kubernetes.Interface, nodeName string) *HAMiCoreManager {
	m := &HAMiCoreManager{
		nvdevlib:     deviceLib,
		hostHookPath: hostHookPath,
		nodeName:     nodeName,
		stopCh:       make(chan struct{}),
	}
	if clientset != nil {
		m.podInformerFactory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Minute,
			informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
				lo.FieldSelector = "spec.nodeName=" + nodeName
			}),
		)
		podInformer := m.podInformerFactory.Core().V1().Pods()
		m.podLister = podInformer.Lister()
		m.podListerSynced = podInformer.Informer().HasSynced
		m.podInformerFactory.Start(m.stopCh)
	}
	return m
}

// WaitForPodCacheSync blocks until the local Pod cache has synced for the first time.
func (m *HAMiCoreManager) WaitForPodCacheSync(ctx context.Context) bool {
	if m.podListerSynced == nil {
		return true
	}
	return cache.WaitForCacheSync(ctx.Done(), m.podListerSynced)
}

func (m *HAMiCoreManager) Stop() {
	close(m.stopCh)
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

// resolveClaimToPod searches the local Pod informer cache for the Pod that
// reserved the given claim.  HAMi DRA guarantees a 1:1 claim-to-container
// binding, so it also returns the exact container name.
func (m *HAMiCoreManager) resolveClaimToPod(claim *resourceapi.ResourceClaim) (*corev1.Pod, string, error) {
	if m.podLister == nil {
		return nil, "", fmt.Errorf("pod lister not initialized")
	}
	if len(claim.Status.ReservedFor) == 0 {
		return nil, "", fmt.Errorf("claim %s has no ReservedFor entries", claim.UID)
	}

	// Find the Pod that reserved this claim.
	consumer := claim.Status.ReservedFor[0]
	if consumer.Resource != "pods" {
		return nil, "", fmt.Errorf("claim %s reservedFor[0] is not a Pod", claim.UID)
	}

	pod, err := m.podLister.Pods(claim.Namespace).Get(consumer.Name)
	if err != nil {
		return nil, "", fmt.Errorf("pod %s/%s not found in local cache: %w", claim.Namespace, consumer.Name, err)
	}

	// HAMi DRA design guarantees one claim per container, but we defensively
	// iterate over all containers and init containers.
	var containerName string
	for _, c := range pod.Spec.Containers {
		for _, rc := range c.Resources.Claims {
			if rc.Name == claim.Name {
				containerName = c.Name
				break
			}
		}
		if containerName != "" {
			break
		}
	}
	if containerName == "" {
		for _, c := range pod.Spec.InitContainers {
			for _, rc := range c.Resources.Claims {
				if rc.Name == claim.Name {
					containerName = c.Name
					break
				}
			}
			if containerName != "" {
				break
			}
		}
	}
	if containerName == "" {
		return nil, "", fmt.Errorf("no container in pod %s/%s references claim %s", claim.Namespace, pod.Name, claim.Name)
	}

	return pod, containerName, nil
}

func (m *HAMiCoreManager) GetCDIContainerEdits(claim *resourceapi.ResourceClaim, devs AllocatableDevices) *cdiapi.ContainerEdits {
	pod, containerName, err := m.resolveClaimToPod(claim)
	if err != nil {
		klog.Warningf("HAMiCoreManager: cannot resolve claim %s to pod/container: %v", claim.UID, err)
		// Fallback to claim-scoped directory so that Prepare does not hard-fail.
		// Metrics will be incomplete, but the workload can still run.
		pod = &corev1.Pod{}
		pod.UID = claim.UID
		containerName = "unknown"
	}

	podUID := string(pod.UID)
	cacheFileHostDirectory := filepath.Join(m.hostHookPath, "vgpu", "containers", podUID+"_"+containerName)
	cacheFilePath := filepath.Join(cacheFileHostDirectory, string(claim.UID)+".cache")

	// Clean up and recreate the directory for this pod+container.
	if err := os.RemoveAll(cacheFileHostDirectory); err != nil {
		klog.Warningf("Failed to remove host directory for cachefile %s: %v", cacheFileHostDirectory, err)
	}
	if err := os.MkdirAll(cacheFileHostDirectory, 0777); err != nil {
		klog.Warningf("Failed to create host directory for cachefile %s: %v", cacheFileHostDirectory, err)
	}
	if err := os.Chmod(cacheFileHostDirectory, 0777); err != nil {
		klog.Warningf("Failed to chmod host directory for cachefile %s: %v", cacheFileHostDirectory, err)
	}

	hamiEnvs := []string{}
	// TOOD: Get SM Limit from Claim's Annotation
	hamiEnvs = append(hamiEnvs, fmt.Sprintf("CUDA_DEVICE_MEMORY_SHARED_CACHE=%s", cacheFilePath))

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
					ContainerPath: filepath.Join(m.hostHookPath, "vgpu", "libvgpu.so"),
					HostPath:      filepath.Join(m.hostHookPath, "vgpu", "libvgpu.so"),
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				// TODO: Check CUDA_DISABLE_CONTROL env before mount ld.so.preload
				{
					ContainerPath: "/etc/ld.so.preload",
					HostPath:      filepath.Join(m.hostHookPath, "vgpu", "ld.so.preload"),
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
	containersPath := filepath.Join(m.hostHookPath, "vgpu", "containers")
	entries, err := os.ReadDir(containersPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to list containers path %s: %w", containersPath, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		claimCache := filepath.Join(containersPath, entry.Name(), claimUID+".cache")
		if _, err := os.Stat(claimCache); err == nil {
			dirToRemove := filepath.Join(containersPath, entry.Name())
			if err := os.RemoveAll(dirToRemove); err != nil {
				return fmt.Errorf("failed to remove container cache directory %s: %w", dirToRemove, err)
			}
			klog.V(4).Infof("Unprepare: removed HAMi-Core cache directory %s for claim %s", dirToRemove, claimUID)
			return nil
		}
	}
	klog.V(4).Infof("Unprepare: no HAMi-Core cache directory found for claim %s", claimUID)
	return nil
}

// For types.go
const HAMiGpuDeviceType = "hami-gpu"
