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
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Masterminds/semver"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
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

// hookPath is the hook tree as addressed inside containers: inside this
// plugin's own container, where the tree is mounted and written to, and inside
// the workload containers, where the CDI mounts land. The baked-in
// ld.so.preload refers to it as well, so it is not configurable.
const hookPath = "/usr/local"

// hostHookPathEnvVar overrides the host-side location of the hook tree for
// hosts whose /usr/local is not writable. The volume backing it must still be
// mounted at hookPath in this plugin's container.
const hostHookPathEnvVar = "HAMI_HOST_HOOK_PATH"

// Upstream vGPUmonitor uses this legacy label to select Pods for container metrics.
const hamiMonitorNodeLabel = "hami.io/vgpu-node"

// Persist claim ownership before CUDA creates its cache. The monitor only
// scans *.cache files, so this marker must not use that suffix.
const hamiCoreClaimMarkerPrefix = ".claim-"

// For sharing.go
type HAMiCoreManager struct {
	// hostHookPath addresses the same tree as hookPath, but as seen from the
	// host: CDI mount sources are resolved in the host mount namespace.
	hostHookPath string
	// containerHookPath is fixed to hookPath in production. Tests use a
	// temporary tree to exercise cache operations without writing to /usr/local.
	containerHookPath string
	nvdevlib          *deviceLib
	nodeName          string
	clientset         kubernetes.Interface

	podInformerFactory informers.SharedInformerFactory
	podLister          corelisters.PodLister
	podListerSynced    cache.InformerSynced
	stopCh             chan struct{}
	stopOnce           sync.Once
}

func NewHAMiCoreManager(deviceLib *deviceLib, hostHookPath string, clientset kubernetes.Interface, nodeName string) *HAMiCoreManager {
	m := &HAMiCoreManager{
		nvdevlib:          deviceLib,
		hostHookPath:      filepath.Clean(hostHookPath),
		containerHookPath: hookPath,
		nodeName:          nodeName,
		clientset:         clientset,
		stopCh:            make(chan struct{}),
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

// waitForPodCacheSyncAtStartup bounds the best-effort wait without stopping the
// informer on timeout. Process cancellation instead stops it and aborts startup.
func (m *HAMiCoreManager) waitForPodCacheSyncAtStartup(ctx context.Context) error {
	const timeout = 30 * time.Second
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	synced := m.WaitForPodCacheSync(syncCtx)
	if err := ctx.Err(); err != nil {
		m.Stop()
		return err
	}
	if !synced {
		klog.Warningf("HAMiCoreManager Pod cache did not sync within %s; continuing with live API lookups while the informer retries", timeout)
	}
	return nil
}

// Stop signals the informer to stop and waits for its workers and handlers.
// All callers, including concurrent/repeated calls, wait for the same shutdown.
func (m *HAMiCoreManager) Stop() {
	m.stopOnce.Do(func() {
		if m.stopCh != nil {
			close(m.stopCh)
		}
		if m.podInformerFactory != nil {
			m.podInformerFactory.Shutdown()
		}
	})
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

func podClaimAliasForResourceClaim(pod *corev1.Pod, resourceClaimName string) (string, error) {
	for _, podClaim := range pod.Spec.ResourceClaims {
		if podClaim.ResourceClaimName != nil && *podClaim.ResourceClaimName == resourceClaimName {
			return podClaim.Name, nil
		}

		if podClaim.ResourceClaimTemplateName == nil {
			continue
		}
		for _, claimStatus := range pod.Status.ResourceClaimStatuses {
			if claimStatus.Name == podClaim.Name && claimStatus.ResourceClaimName != nil && *claimStatus.ResourceClaimName == resourceClaimName {
				return podClaim.Name, nil
			}
		}
	}

	return "", fmt.Errorf("pod %s/%s does not reference ResourceClaim %s", pod.Namespace, pod.Name, resourceClaimName)
}

func containerNameForPodClaimAlias(containers []corev1.Container, claimAlias string) string {
	for _, container := range containers {
		for _, claim := range container.Resources.Claims {
			if claim.Name == claimAlias {
				return container.Name
			}
		}
	}
	return ""
}

// resolveClaimToPod searches the local Pod informer cache, falling back to a
// live lookup on a cache miss. Validate the reserving Pod's UID and node before
// mapping the claim to its regular or init container.
func (m *HAMiCoreManager) resolveClaimToPod(ctx context.Context, claim *resourceapi.ResourceClaim) (*corev1.Pod, string, error) {
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
	if apierrors.IsNotFound(err) && m.clientset != nil {
		pod, err = m.clientset.CoreV1().Pods(claim.Namespace).Get(ctx, consumer.Name, metav1.GetOptions{})
		if err != nil {
			return nil, "", fmt.Errorf("get pod %s/%s after local cache miss: %w", claim.Namespace, consumer.Name, err)
		}
	}
	if err != nil {
		return nil, "", fmt.Errorf("pod %s/%s not found in local cache: %w", claim.Namespace, consumer.Name, err)
	}
	if pod.UID != consumer.UID {
		return nil, "", fmt.Errorf("pod %s/%s UID %s does not match claim consumer UID %s", claim.Namespace, consumer.Name, pod.UID, consumer.UID)
	}
	if m.nodeName == "" || pod.Spec.NodeName != m.nodeName {
		return nil, "", fmt.Errorf("pod %s/%s is assigned to node %q, not %q", claim.Namespace, consumer.Name, pod.Spec.NodeName, m.nodeName)
	}

	claimAlias, err := podClaimAliasForResourceClaim(pod, claim.Name)
	if err != nil {
		return nil, "", err
	}

	// HAMi DRA design guarantees one claim per container, but check both
	// regular and init containers to resolve the exact consumer.
	containerName := containerNameForPodClaimAlias(pod.Spec.Containers, claimAlias)
	if containerName == "" {
		containerName = containerNameForPodClaimAlias(pod.Spec.InitContainers, claimAlias)
	}
	if containerName == "" {
		return nil, "", fmt.Errorf("no container in pod %s/%s references PodResourceClaim %s for ResourceClaim %s", claim.Namespace, pod.Name, claimAlias, claim.Name)
	}

	return pod, containerName, nil
}

// ensurePodMonitorLabel uses the live Pod, not the informer snapshot, so retries
// and other claims on the same Pod see the latest labels. UID and resourceVersion
// in the patch protect against Pod replacement and concurrent metadata changes.
func (m *HAMiCoreManager) ensurePodMonitorLabel(ctx context.Context, pod *corev1.Pod) error {
	if m.clientset == nil || m.nodeName == "" {
		return fmt.Errorf("pod client and node name are required for vGPUmonitor labeling")
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		live, err := m.clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if live.UID != pod.UID {
			return fmt.Errorf("pod UID changed from %s to %s", pod.UID, live.UID)
		}
		if live.Spec.NodeName != m.nodeName {
			return fmt.Errorf("pod is assigned to node %q, not %q", live.Spec.NodeName, m.nodeName)
		}
		if current := live.Labels[hamiMonitorNodeLabel]; current != "" {
			if current != m.nodeName {
				return fmt.Errorf("conflicting %s label %q, expected %q", hamiMonitorNodeLabel, current, m.nodeName)
			}
			return nil
		}
		patch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"uid": live.UID, "resourceVersion": live.ResourceVersion,
				"labels": map[string]string{hamiMonitorNodeLabel: m.nodeName},
			},
		})
		if err != nil {
			return err
		}
		_, err = m.clientset.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("label Pod %s/%s for vGPUmonitor: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

func (m *HAMiCoreManager) GetCDIContainerEdits(ctx context.Context, claim *resourceapi.ResourceClaim, devs AllocatableDevices) (*cdiapi.ContainerEdits, error) {
	pod, containerName, err := m.resolveClaimToPod(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("resolve claim %s to pod/container: %w", claim.UID, err)
	}
	if err := m.ensurePodMonitorLabel(ctx, pod); err != nil {
		// Do not alter cache directories or return successful preparation when
		// a resolved workload cannot be made discoverable by the monitor.
		return nil, err
	}

	podUID := string(pod.UID)
	cacheFileHostDirectory := filepath.Join(m.hostHookPath, "vgpu", "containers", podUID+"_"+containerName)
	cacheFileDirectory := filepath.Join(m.containerHookPath, "vgpu", "containers", podUID+"_"+containerName)
	cacheFilePath := filepath.Join(cacheFileDirectory, string(claim.UID)+".cache")

	// Clean up and recreate the directory for this pod+container.
	if err := os.RemoveAll(cacheFileDirectory); err != nil {
		return nil, fmt.Errorf("remove cache directory %s: %w", cacheFileDirectory, err)
	}
	if err := os.MkdirAll(cacheFileDirectory, 0777); err != nil {
		return nil, fmt.Errorf("create cache directory %s: %w", cacheFileDirectory, err)
	}
	if err := os.Chmod(cacheFileDirectory, 0777); err != nil {
		return nil, fmt.Errorf("chmod cache directory %s: %w", cacheFileDirectory, err)
	}
	marker := filepath.Join(cacheFileDirectory, hamiCoreClaimMarkerPrefix+string(claim.UID))
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		return nil, fmt.Errorf("record claim ownership in %s: %w", marker, err)
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
					ContainerPath: cacheFileDirectory,
					HostPath:      cacheFileHostDirectory,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: filepath.Join(hookPath, "vgpu", "libvgpu.so"),
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
	}, nil
}

// Unprepare removes cache state at claim release or abandoned preparation, not
// at startup or during layout migration: legacy mounts may still be in use.
func (m *HAMiCoreManager) Unprepare(claimUID string, pl PreparedDeviceList) error {
	if !filepath.IsLocal(claimUID) || filepath.Base(claimUID) != claimUID || claimUID == "." {
		return fmt.Errorf("invalid claim UID %q for cache cleanup", claimUID)
	}

	// Completed pre-upgrade claims retain their original CDI cache mounts.
	// Remove only this claim's legacy path, even if containers/ does not exist.
	legacyDirectory := filepath.Join(m.containerHookPath, "vgpu", "claims", claimUID)
	if err := os.RemoveAll(legacyDirectory); err != nil {
		return fmt.Errorf("remove legacy claim cache directory %s: %w", legacyDirectory, err)
	}

	containersPath := filepath.Join(m.containerHookPath, "vgpu", "containers")
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
		directory := filepath.Join(containersPath, entry.Name())
		owned := false
		// Keep accepting cache-file ownership for preparations made before the
		// marker was introduced. Never infer ownership from an empty directory.
		for _, name := range []string{hamiCoreClaimMarkerPrefix + claimUID, claimUID + ".cache"} {
			path := filepath.Join(directory, name)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect claim ownership %s: %w", path, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("claim ownership path %s is not a regular file", path)
			}
			owned = true
			break
		}
		if !owned {
			continue
		}
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("failed to remove container cache directory %s: %w", directory, err)
		}
		klog.V(4).Infof("Unprepare: removed HAMi-Core cache directory %s for claim %s", directory, claimUID)
	}
	return nil
}

// For types.go
const HAMiGpuDeviceType = "hami-gpu"
