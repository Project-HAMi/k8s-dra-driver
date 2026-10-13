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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

func TestPodClaimAliasForResourceClaim(t *testing.T) {
	tests := []struct {
		name              string
		pod               *corev1.Pod
		resourceClaimName string
		wantAlias         string
		wantError         bool
	}{
		{
			name: "direct claim",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{
					Name:              "gpu",
					ResourceClaimName: ptr.To("gpu-claim"),
				}}},
			},
			resourceClaimName: "gpu-claim",
			wantAlias:         "gpu",
		},
		{
			name: "template-generated claim",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{
					Name:                      "gpu",
					ResourceClaimTemplateName: ptr.To("gpu-template"),
				}}},
				Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{{
					Name:              "gpu",
					ResourceClaimName: ptr.To("pod-gpu-generated-abcde"),
				}}},
			},
			resourceClaimName: "pod-gpu-generated-abcde",
			wantAlias:         "gpu",
		},
		{
			name: "template-generated claim status missing",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "workload"},
				Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{
					Name:                      "gpu",
					ResourceClaimTemplateName: ptr.To("gpu-template"),
				}}},
			},
			resourceClaimName: "pod-gpu-generated-abcde",
			wantError:         true,
		},
		{
			name: "claim not referenced",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "workload"},
				Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{
					Name:              "gpu",
					ResourceClaimName: ptr.To("other-claim"),
				}}},
			},
			resourceClaimName: "gpu-claim",
			wantError:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := podClaimAliasForResourceClaim(tt.pod, tt.resourceClaimName)
			if tt.wantError {
				if err == nil {
					t.Fatalf("podClaimAliasForResourceClaim() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("podClaimAliasForResourceClaim() error = %v", err)
			}
			if got != tt.wantAlias {
				t.Errorf("podClaimAliasForResourceClaim() = %q, want %q", got, tt.wantAlias)
			}
		})
	}
}

func TestContainerNameForPodClaimAlias(t *testing.T) {
	containers := []corev1.Container{
		{
			Name: "unrelated",
			Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
				Name: "other",
			}}},
		},
		{
			Name: "gpu-workload",
			Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
				Name: "gpu",
			}}},
		},
	}

	if got := containerNameForPodClaimAlias(containers, "gpu"); got != "gpu-workload" {
		t.Errorf("containerNameForPodClaimAlias() = %q, want %q", got, "gpu-workload")
	}
	if got := containerNameForPodClaimAlias(containers, "missing"); got != "" {
		t.Errorf("containerNameForPodClaimAlias() = %q, want empty string", got)
	}
}

func TestResolveClaimToPod(t *testing.T) {
	tests := []struct {
		name           string
		podClaim       corev1.PodResourceClaim
		claimStatus    []corev1.PodResourceClaimStatus
		containers     []corev1.Container
		initContainers []corev1.Container
		claimName      string
		wantContainer  string
	}{
		{
			name: "direct claim used by regular container",
			podClaim: corev1.PodResourceClaim{
				Name:              "gpu",
				ResourceClaimName: ptr.To("gpu-claim"),
			},
			containers: []corev1.Container{{
				Name: "workload",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
					Name: "gpu",
				}}},
			}},
			claimName:     "gpu-claim",
			wantContainer: "workload",
		},
		{
			name: "template-generated claim used by init container",
			podClaim: corev1.PodResourceClaim{
				Name:                      "gpu",
				ResourceClaimTemplateName: ptr.To("gpu-template"),
			},
			claimStatus: []corev1.PodResourceClaimStatus{{
				Name:              "gpu",
				ResourceClaimName: ptr.To("generated-gpu-abcde"),
			}},
			initContainers: []corev1.Container{{
				Name: "gpu-init",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
					Name: "gpu",
				}}},
			}},
			claimName:     "generated-gpu-abcde",
			wantContainer: "gpu-init",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "pod",
					UID:       types.UID("pod-uid"),
				},
				Spec: corev1.PodSpec{
					ResourceClaims: []corev1.PodResourceClaim{tt.podClaim},
					Containers:     tt.containers,
					InitContainers: tt.initContainers,
				},
				Status: corev1.PodStatus{ResourceClaimStatuses: tt.claimStatus},
			}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
				cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			})
			if err := indexer.Add(pod); err != nil {
				t.Fatalf("add Pod to indexer: %v", err)
			}
			manager := &HAMiCoreManager{podLister: corelisters.NewPodLister(indexer)}
			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      tt.claimName,
					UID:       types.UID("claim-uid"),
				},
				Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{{
					Resource: "pods",
					Name:     pod.Name,
					UID:      pod.UID,
				}}},
			}

			gotPod, gotContainer, err := manager.resolveClaimToPod(claim)
			if err != nil {
				t.Fatalf("resolveClaimToPod() error = %v", err)
			}
			if gotPod.UID != pod.UID {
				t.Errorf("resolveClaimToPod() Pod UID = %q, want %q", gotPod.UID, pod.UID)
			}
			if gotContainer != tt.wantContainer {
				t.Errorf("resolveClaimToPod() container = %q, want %q", gotContainer, tt.wantContainer)
			}
		})
	}
}

func TestResolveClaimToPodErrors(t *testing.T) {
	basePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod",
			UID:       types.UID("pod-uid"),
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:              "gpu",
				ResourceClaimName: ptr.To("gpu-claim"),
			}},
			Containers: []corev1.Container{{
				Name: "workload",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
					Name: "gpu",
				}}},
			}},
		},
	}
	podWithoutConsumer := basePod.DeepCopy()
	podWithoutConsumer.Spec.Containers = nil

	tests := []struct {
		name          string
		manager       *HAMiCoreManager
		claim         *resourceapi.ResourceClaim
		wantErrorText string
	}{
		{
			name:          "pod lister not initialized",
			manager:       &HAMiCoreManager{},
			claim:         newResourceClaim("gpu-claim", nil),
			wantErrorText: "pod lister not initialized",
		},
		{
			name:          "claim has no consumers",
			manager:       managerWithPods(t, basePod),
			claim:         newResourceClaim("gpu-claim", nil),
			wantErrorText: "has no ReservedFor entries",
		},
		{
			name:    "consumer is not a pod",
			manager: managerWithPods(t, basePod),
			claim: newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{
				Resource: "deployments",
				Name:     "pod",
				UID:      basePod.UID,
			}}),
			wantErrorText: "is not a Pod",
		},
		{
			name:    "pod not found",
			manager: managerWithPods(t),
			claim: newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{
				Resource: "pods",
				Name:     "pod",
				UID:      basePod.UID,
			}}),
			wantErrorText: "not found in local cache",
		},
		{
			name:    "recreated pod has different UID",
			manager: managerWithPods(t, basePod),
			claim: newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{
				Resource: "pods",
				Name:     "pod",
				UID:      types.UID("old-pod-uid"),
			}}),
			wantErrorText: "does not match claim consumer UID",
		},
		{
			name:    "pod does not reference claim",
			manager: managerWithPods(t, basePod),
			claim: newResourceClaim("other-claim", []resourceapi.ResourceClaimConsumerReference{{
				Resource: "pods",
				Name:     "pod",
				UID:      basePod.UID,
			}}),
			wantErrorText: "does not reference ResourceClaim",
		},
		{
			name:    "no container references pod claim alias",
			manager: managerWithPods(t, podWithoutConsumer),
			claim: newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{
				Resource: "pods",
				Name:     "pod",
				UID:      basePod.UID,
			}}),
			wantErrorText: "no container",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.manager.resolveClaimToPod(tt.claim)
			if err == nil {
				t.Fatalf("resolveClaimToPod() error = nil, want error containing %q", tt.wantErrorText)
			}
			if !strings.Contains(err.Error(), tt.wantErrorText) {
				t.Errorf("resolveClaimToPod() error = %q, want it to contain %q", err, tt.wantErrorText)
			}
		})
	}
}

func TestGetCDIContainerEditsCachePath(t *testing.T) {
	hostHookPath := t.TempDir()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod",
			UID:       types.UID("pod-uid"),
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:              "gpu",
				ResourceClaimName: ptr.To("gpu-claim"),
			}},
			Containers: []corev1.Container{{
				Name: "workload",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
					Name: "gpu",
				}}},
			}},
		},
	}
	manager := managerWithPods(t, pod)
	manager.hostHookPath = "/var/lib/hami"
	manager.containerHookPath = hostHookPath
	claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{
		Resource: "pods",
		Name:     pod.Name,
		UID:      pod.UID,
	}})
	claim.Status.Allocation.Devices.Results = []resourceapi.DeviceRequestAllocationResult{{
		Device: "hami-gpu-0",
		ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
			"cores":  resource.MustParse("30"),
			"memory": resource.MustParse("4Gi"),
		},
	}}
	devices := AllocatableDevices{
		"hami-gpu-0": {
			HAMiGpu: &HAMiGpuInfo{GpuInfo: GpuInfo{memoryBytes: 16 * 1024 * 1024 * 1024}},
		},
	}
	wantDirectory := filepath.Join(hostHookPath, "vgpu", "containers", "pod-uid_workload")
	if err := os.MkdirAll(wantDirectory, 0o755); err != nil {
		t.Fatalf("create stale cache directory: %v", err)
	}
	stalePath := filepath.Join(wantDirectory, "stale.cache")
	if err := os.WriteFile(stalePath, nil, 0o644); err != nil {
		t.Fatalf("create stale cache file: %v", err)
	}

	edits := manager.GetCDIContainerEdits(claim, devices)
	wantCache := filepath.Join(wantDirectory, "claim-uid.cache")

	wantEnv := []string{
		"CUDA_DEVICE_MEMORY_SHARED_CACHE=" + wantCache,
		"CUDA_DEVICE_SM_LIMIT_0=30",
		"CUDA_DEVICE_MEMORY_LIMIT_0=4096m",
	}
	if len(edits.Env) != len(wantEnv) {
		t.Fatalf("GetCDIContainerEdits() env = %v, want %v", edits.Env, wantEnv)
	}
	for i := range wantEnv {
		if edits.Env[i] != wantEnv[i] {
			t.Errorf("GetCDIContainerEdits() env[%d] = %q, want %q", i, edits.Env[i], wantEnv[i])
		}
	}
	if len(edits.Mounts) != 4 {
		t.Fatalf("GetCDIContainerEdits() mounts = %d, want 4", len(edits.Mounts))
	}
	wantHostDirectory := filepath.Join(manager.hostHookPath, "vgpu", "containers", "pod-uid_workload")
	if edits.Mounts[0].HostPath != wantHostDirectory || edits.Mounts[0].ContainerPath != wantDirectory {
		t.Errorf("cache mount = %#v, want host path %q and container path %q", edits.Mounts[0], wantHostDirectory, wantDirectory)
	}
	if edits.Mounts[1].ContainerPath != filepath.Join(hookPath, "vgpu", "libvgpu.so") {
		t.Errorf("libvgpu container path = %q, want fixed preload path", edits.Mounts[1].ContainerPath)
	}
	if edits.Mounts[1].HostPath != filepath.Join(manager.hostHookPath, "vgpu", "libvgpu.so") {
		t.Errorf("libvgpu mount host path = %q", edits.Mounts[1].HostPath)
	}
	if edits.Mounts[2].HostPath != filepath.Join(manager.hostHookPath, "vgpu", "ld.so.preload") {
		t.Errorf("ld.so.preload mount host path = %q", edits.Mounts[2].HostPath)
	}
	info, err := os.Stat(wantDirectory)
	if err != nil {
		t.Fatalf("stat cache directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("cache path %q is not a directory", wantDirectory)
	}
	if info.Mode().Perm() != 0o777 {
		t.Errorf("cache directory mode = %o, want 777", info.Mode().Perm())
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale cache file was not removed: %v", err)
	}
}

func TestUnprepareCacheDirectory(t *testing.T) {
	t.Run("missing containers directory is idempotent", func(t *testing.T) {
		manager := &HAMiCoreManager{containerHookPath: t.TempDir()}
		if err := manager.Unprepare("claim-uid", nil); err != nil {
			t.Fatalf("Unprepare() error = %v", err)
		}
	})

	t.Run("removes matching directory and preserves unrelated directories", func(t *testing.T) {
		hostHookPath := t.TempDir()
		containersPath := filepath.Join(hostHookPath, "vgpu", "containers")
		matchingDirectory := filepath.Join(containersPath, "pod-uid_workload")
		unrelatedDirectory := filepath.Join(containersPath, "other-pod_workload")
		if err := os.MkdirAll(matchingDirectory, 0o755); err != nil {
			t.Fatalf("create matching directory: %v", err)
		}
		if err := os.MkdirAll(unrelatedDirectory, 0o755); err != nil {
			t.Fatalf("create unrelated directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(matchingDirectory, "claim-uid.cache"), nil, 0o644); err != nil {
			t.Fatalf("create matching cache: %v", err)
		}
		if err := os.WriteFile(filepath.Join(unrelatedDirectory, "other-claim.cache"), nil, 0o644); err != nil {
			t.Fatalf("create unrelated cache: %v", err)
		}

		manager := &HAMiCoreManager{hostHookPath: hostHookPath, containerHookPath: hostHookPath}
		if err := manager.Unprepare("claim-uid", nil); err != nil {
			t.Fatalf("Unprepare() error = %v", err)
		}
		if _, err := os.Stat(matchingDirectory); !os.IsNotExist(err) {
			t.Errorf("matching directory still exists or stat failed unexpectedly: %v", err)
		}
		if _, err := os.Stat(unrelatedDirectory); err != nil {
			t.Errorf("unrelated directory was removed: %v", err)
		}
	})

	t.Run("missing claim cache is idempotent", func(t *testing.T) {
		hostHookPath := t.TempDir()
		unrelatedDirectory := filepath.Join(hostHookPath, "vgpu", "containers", "pod-uid_workload")
		if err := os.MkdirAll(unrelatedDirectory, 0o755); err != nil {
			t.Fatalf("create unrelated directory: %v", err)
		}

		manager := &HAMiCoreManager{hostHookPath: hostHookPath, containerHookPath: hostHookPath}
		if err := manager.Unprepare("missing-claim", nil); err != nil {
			t.Fatalf("Unprepare() error = %v", err)
		}
		if _, err := os.Stat(unrelatedDirectory); err != nil {
			t.Errorf("unrelated directory was removed: %v", err)
		}
	})

	t.Run("returns containers path read error", func(t *testing.T) {
		hostHookPath := t.TempDir()
		containersPath := filepath.Join(hostHookPath, "vgpu", "containers")
		if err := os.MkdirAll(filepath.Dir(containersPath), 0o755); err != nil {
			t.Fatalf("create parent directory: %v", err)
		}
		if err := os.WriteFile(containersPath, nil, 0o644); err != nil {
			t.Fatalf("create containers path as file: %v", err)
		}

		manager := &HAMiCoreManager{hostHookPath: hostHookPath, containerHookPath: hostHookPath}
		if err := manager.Unprepare("claim-uid", nil); err == nil || !strings.Contains(err.Error(), "failed to list containers path") {
			t.Fatalf("Unprepare() error = %v, want containers path error", err)
		}
	})
}

func managerWithPods(t *testing.T, pods ...*corev1.Pod) *HAMiCoreManager {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("add Pod to indexer: %v", err)
		}
	}
	return &HAMiCoreManager{podLister: corelisters.NewPodLister(indexer)}
}

func newResourceClaim(name string, reservedFor []resourceapi.ResourceClaimConsumerReference) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			UID:       types.UID("claim-uid"),
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation:  &resourceapi.AllocationResult{},
			ReservedFor: reservedFor,
		},
	}
}

func TestNewHAMiCoreManagerHostHookPath(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "unset", env: "", want: "/usr/local"},
		{name: "custom", env: "/var/lib/hami", want: "/var/lib/hami"},
		{name: "trailing slash", env: "/var/lib/hami/", want: "/var/lib/hami"},
		{name: "root", env: "/", want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configuredPath := tt.env
			if configuredPath == "" {
				configuredPath = hookPath
			}
			manager := NewHAMiCoreManager(nil, configuredPath, nil, "")
			t.Cleanup(manager.Stop)
			require.Equal(t, tt.want, manager.hostHookPath)
			require.Equal(t, hookPath, manager.containerHookPath)
		})
	}
}
