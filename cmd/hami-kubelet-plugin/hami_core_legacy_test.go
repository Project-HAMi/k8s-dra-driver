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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"

	"github.com/Project-HAMi/k8s-dra-driver/pkg/featuregates"
)

func TestUnprepareLegacyClaimDirectory(t *testing.T) {
	for _, withContainers := range []bool{false, true} {
		name := "legacy only"
		if withContainers {
			name = "mixed layouts"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			legacy := filepath.Join(root, "vgpu", "claims", "claim-uid")
			other := filepath.Join(root, "vgpu", "claims", "other-claim")
			for _, dir := range []string{legacy, other} {
				require.NoError(t, os.MkdirAll(dir, 0755))
				// Old preparations used random cache filenames, not the claim UID.
				require.NoError(t, os.WriteFile(filepath.Join(dir, "random.cache"), []byte("cache"), 0600))
			}
			modern := filepath.Join(root, "vgpu", "containers", "pod_workload")
			otherModern := filepath.Join(root, "vgpu", "containers", "other-pod_workload")
			if withContainers {
				for _, dir := range []string{modern, otherModern} {
					require.NoError(t, os.MkdirAll(dir, 0755))
				}
				require.NoError(t, os.WriteFile(filepath.Join(modern, hamiCoreClaimMarkerPrefix+"claim-uid"), nil, 0600))
				require.NoError(t, os.WriteFile(filepath.Join(otherModern, hamiCoreClaimMarkerPrefix+"other-claim"), nil, 0600))
			}
			manager := &HAMiCoreManager{containerHookPath: root, hostHookPath: "/different-host-view"}
			require.NoError(t, manager.Unprepare("claim-uid", nil))
			require.NoDirExists(t, legacy)
			require.FileExists(t, filepath.Join(other, "random.cache"))
			if withContainers {
				require.NoDirExists(t, modern)
				require.FileExists(t, filepath.Join(otherModern, hamiCoreClaimMarkerPrefix+"other-claim"))
			}
			require.NoError(t, manager.Unprepare("claim-uid", nil), "cleanup must be idempotent")
		})
	}
}

func TestLegacyClaimDirectoryPreservedUntilUnprepare(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	legacy := filepath.Join(root, "vgpu", "claims", testClaimUID)
	require.NoError(t, os.MkdirAll(legacy, 0755))
	cache := filepath.Join(legacy, "random.cache")
	require.NoError(t, os.WriteFile(cache, []byte("active legacy cache"), 0600))
	before, err := os.Stat(cache)
	require.NoError(t, err)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	manager := NewHAMiCoreManager(nil, root, client, "node-a")
	defer manager.Stop()
	manager.containerHookPath = root
	s := legacyCleanupTestState(t, root)
	s.hamiCoreManager = manager
	completed := completedHAMiClaim()
	completed.PreparedDevices[0].Devices[0].HAMiGpu.Info = &HAMiGpuInfo{}
	completed.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}}
	// Model an upgrade from a checkpoint without a boot ID. Neither boot-ID
	// migration nor same-boot restart may delete caches of running workloads.
	require.NoError(t, s.createCheckpoint(ctx, &Checkpoint{V2: &CheckpointV2{
		PreparedClaims: PreparedClaimsByUID{testClaimUID: completed},
	}}))
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim", UID: testClaimUID}}
	for range 2 {
		require.NoError(t, s.initCheckpoint(ctx, testBootID))
		cleanup := NewCheckpointCleanupManager(s, nil)
		cleanup.cleanup(ctx) // completed claims must not be startup-GC candidates
		devices, err := s.Prepare(ctx, claim)
		require.NoError(t, err)
		require.Len(t, devices, 1)
		require.Equal(t, []string{testCDIDeviceID}, devices[0].CDIDeviceIDs)
		after, err := os.Stat(cache)
		require.NoError(t, err)
		require.True(t, os.SameFile(before, after), "Prepare/restart must retain the original cache inode")
		data, err := os.ReadFile(cache)
		require.NoError(t, err)
		require.Equal(t, "active legacy cache", string(data))
	}
	manager.Stop()
	require.FileExists(t, cache, "driver/informer shutdown is not claim release")

	// Kubelet sends Unprepare after containers stop; the Pod's API phase may
	// still be Running until Unprepare completes, so do not gate on that phase.
	ref := kubeletplugin.NamespacedObject{
		UID: types.UID(testClaimUID), NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim"},
	}
	require.NoError(t, s.Unprepare(ctx, ref))
	require.NoDirExists(t, legacy)
	cp, err := s.getCheckpoint(ctx)
	require.NoError(t, err)
	require.NotContains(t, cp.V2.PreparedClaims, testClaimUID)
}

func TestLegacyClaimCleanupFailureCanRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "vgpu"), 0755))
	claimsRoot := filepath.Join(root, "vgpu", "claims")
	// A symlink loop gives a deterministic filesystem error even as root.
	require.NoError(t, os.Symlink("claims", claimsRoot))
	modern := filepath.Join(root, "vgpu", "containers", "pod_workload")
	require.NoError(t, os.MkdirAll(modern, 0755))
	marker := filepath.Join(modern, hamiCoreClaimMarkerPrefix+testClaimUID)
	require.NoError(t, os.WriteFile(marker, nil, 0600))
	s := legacyCleanupTestState(t, root)
	completed := completedHAMiClaim()
	completed.PreparedDevices[0].Devices[0].HAMiGpu.Info = &HAMiGpuInfo{}
	require.NoError(t, s.createCheckpoint(ctx, &Checkpoint{V2: &CheckpointV2{
		NodeBootID: testBootID, PreparedClaims: PreparedClaimsByUID{testClaimUID: completed},
	}}))
	spec := filepath.Join(s.cdi.cdiRoot, cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, testClaimUID)+".yaml")
	require.NoError(t, os.WriteFile(spec, []byte("existing CDI spec"), 0600))
	ref := kubeletplugin.NamespacedObject{UID: types.UID(testClaimUID)}
	require.ErrorContains(t, s.Unprepare(ctx, ref), "legacy claim cache directory")
	cp, err := s.getCheckpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, completed, cp.V2.PreparedClaims[testClaimUID])
	require.FileExists(t, spec, "failed cleanup must not remove the CDI spec")
	require.FileExists(t, marker, "failed legacy cleanup must not erase current ownership")

	// Repair only the test fixture and verify the same Unprepare can finish.
	require.NoError(t, os.Remove(claimsRoot))
	legacy := filepath.Join(claimsRoot, testClaimUID)
	require.NoError(t, os.MkdirAll(legacy, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "random.cache"), nil, 0600))
	require.NoError(t, s.Unprepare(ctx, ref))
	require.NoDirExists(t, legacy)
	require.NoDirExists(t, modern)
	require.NoFileExists(t, spec)
	cp, err = s.getCheckpoint(ctx)
	require.NoError(t, err)
	require.NotContains(t, cp.V2.PreparedClaims, testClaimUID)
}

func TestUnprepareLegacyRejectsUnsafeClaimUID(t *testing.T) {
	for _, uid := range []string{"", ".", "..", "/", "../other-claim", "nested/claim"} {
		t.Run(uid, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "vgpu", "claims", "other-claim", "random.cache")
			require.NoError(t, os.MkdirAll(filepath.Dir(cache), 0755))
			require.NoError(t, os.WriteFile(cache, []byte("preserve"), 0600))
			manager := &HAMiCoreManager{containerHookPath: root}
			require.ErrorContains(t, manager.Unprepare(uid, nil), "invalid claim UID")
			require.FileExists(t, cache)
		})
	}
}

func legacyCleanupTestState(t *testing.T, root string) *DeviceState {
	t.Helper()
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	s := newTestDeviceState(t)
	s.hamiCoreManager = &HAMiCoreManager{containerHookPath: root}
	s.cdi = &CDIHandler{cdiRoot: t.TempDir()}
	return s
}
