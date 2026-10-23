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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

func TestGetCDIContainerEditsLabelsPodForMonitor(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid", Labels: map[string]string{"app": "workload"}},
		Spec: corev1.PodSpec{
			NodeName:       "node-a",
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}},
			Containers:     []corev1.Container{{Name: "workload", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}}},
		},
	}
	client := fake.NewClientset(pod)
	manager := NewHAMiCoreManager(nil, t.TempDir(), client, "node-a")
	manager.containerHookPath = t.TempDir()
	t.Cleanup(manager.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.True(t, manager.WaitForPodCacheSync(ctx))
	claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}})
	edits, err := manager.GetCDIContainerEdits(ctx, claim, nil)
	require.NoError(t, err)
	require.NotNil(t, edits)
	got, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "node-a", got.Labels["hami.io/vgpu-node"])
	require.Equal(t, "workload", got.Labels["app"])
}

func TestGetCDIContainerEditsCacheMissUsesLivePod(t *testing.T) {
	for _, template := range []bool{false, true} {
		name := "direct"
		if template {
			name = "template init container"
		}
		t.Run(name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid"},
				Spec: corev1.PodSpec{NodeName: "node-a",
					ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}},
					Containers: []corev1.Container{{Name: "workload", Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{{Name: "gpu"}},
					}}},
				},
			}
			if template {
				pod.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-template")}}
				pod.Status.ResourceClaimStatuses = []corev1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}}
				pod.Spec.InitContainers, pod.Spec.Containers = pod.Spec.Containers, nil
			}
			client := fake.NewClientset(pod)
			manager := managerWithPods(t) // informer has not observed the Pod yet
			manager.clientset, manager.nodeName = client, "node-a"
			manager.containerHookPath, manager.hostHookPath = t.TempDir(), t.TempDir()
			claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}})
			edits, err := manager.GetCDIContainerEdits(context.Background(), claim, nil)
			require.NoError(t, err)
			require.Contains(t, edits.Env, "CUDA_DEVICE_MEMORY_SHARED_CACHE="+filepath.Join(manager.containerHookPath, "vgpu", "containers", "pod-uid_workload", "claim-uid.cache"))
			got, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "node-a", got.Labels[hamiMonitorNodeLabel])
			require.NoDirExists(t, filepath.Join(manager.containerHookPath, "vgpu", "containers", "claim-uid_unknown"))
		})
	}
}

func TestGetCDIContainerEditsCacheMissFailsWithoutSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*corev1.Pod)
		missing   bool
		getError  error
		wantError string
	}{
		{name: "Pod absent", missing: true, wantError: "not found"},
		{name: "replacement Pod", mutate: func(p *corev1.Pod) { p.UID = "other-uid" }, wantError: "does not match claim consumer UID"},
		{name: "wrong node", mutate: func(p *corev1.Pod) { p.Spec.NodeName = "node-b" }, wantError: "assigned to node"},
		{name: "unbound Pod", mutate: func(p *corev1.Pod) { p.Spec.NodeName = "" }, wantError: "assigned to node"},
		{name: "missing claim alias", mutate: func(p *corev1.Pod) { p.Spec.ResourceClaims = nil }, wantError: "does not reference ResourceClaim"},
		{name: "missing container", mutate: func(p *corev1.Pod) { p.Spec.Containers = nil }, wantError: "no container"},
		{name: "API unavailable", getError: apierrors.NewServiceUnavailable("try again"), wantError: "try again"},
		{name: "API forbidden", getError: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", errors.New("test denied")), wantError: "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid"},
				Spec: corev1.PodSpec{NodeName: "node-a",
					ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}},
					Containers: []corev1.Container{{Name: "workload", Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{{Name: "gpu"}},
					}}},
				},
			}
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			client := fake.NewClientset()
			if !tc.missing {
				_, err := client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			if tc.getError != nil {
				client.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.getError
				})
			}
			manager := managerWithPods(t)
			manager.clientset, manager.nodeName = client, "node-a"
			manager.containerHookPath, manager.hostHookPath = t.TempDir(), t.TempDir()
			claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "pod", UID: "pod-uid"}})
			edits, err := manager.GetCDIContainerEdits(context.Background(), claim, nil)
			require.ErrorContains(t, err, tc.wantError)
			if tc.getError != nil {
				require.ErrorIs(t, err, tc.getError)
			}
			require.Nil(t, edits)
			require.NoDirExists(t, filepath.Join(manager.containerHookPath, "vgpu", "containers"))
			for _, action := range client.Actions() {
				require.NotEqual(t, "patch", action.GetVerb(), "failed resolution must not label a Pod")
			}
		})
	}
}

func TestGetCDIContainerEditsCacheMissCanRetry(t *testing.T) {
	client := fake.NewClientset()
	manager := managerWithPods(t)
	manager.clientset, manager.nodeName = client, "node-a"
	manager.containerHookPath, manager.hostHookPath = t.TempDir(), t.TempDir()
	claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "pod", UID: "pod-uid"}})
	edits, err := manager.GetCDIContainerEdits(context.Background(), claim, nil)
	require.Error(t, err)
	require.Nil(t, edits)
	require.True(t, apierrors.IsNotFound(err))
	require.NoDirExists(t, filepath.Join(manager.containerHookPath, "vgpu", "containers"))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid"},
		Spec: corev1.PodSpec{NodeName: "node-a",
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}},
			Containers: []corev1.Container{{Name: "workload", Resources: corev1.ResourceRequirements{
				Claims: []corev1.ResourceClaim{{Name: "gpu"}},
			}}},
		},
	}
	_, err = client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	// Retry before the informer catches up: the API now has the correct Pod.
	edits, err = manager.GetCDIContainerEdits(context.Background(), claim, nil)
	require.NoError(t, err)
	require.Contains(t, edits.Env, "CUDA_DEVICE_MEMORY_SHARED_CACHE="+filepath.Join(manager.containerHookPath, "vgpu", "containers", "pod-uid_workload", "claim-uid.cache"))
	got, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "node-a", got.Labels[hamiMonitorNodeLabel])
}

func TestGetCDIContainerEditsMonitorLabelSafety(t *testing.T) {
	for _, tc := range []struct {
		name        string
		uid         types.UID
		node, label string
		denyPatch   bool
		wantError   string
	}{
		{name: "nil labels", uid: "pod-uid", node: "node-a"},
		{name: "already labeled needs no write", uid: "pod-uid", node: "node-a", label: "node-a", denyPatch: true},
		{name: "conflicting label", uid: "pod-uid", node: "node-a", label: "node-b", wantError: "conflicting"},
		{name: "replacement Pod", uid: "replacement-uid", node: "node-a", wantError: "UID changed"},
		{name: "wrong node", uid: "pod-uid", node: "node-b", wantError: "assigned to node"},
		{name: "unbound Pod", uid: "pod-uid", wantError: "assigned to node"},
		{name: "permission denied", uid: "pod-uid", node: "node-a", denyPatch: true, wantError: "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid", ResourceVersion: "1"},
				Spec: corev1.PodSpec{NodeName: "node-a",
					ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}},
					Containers:     []corev1.Container{{Name: "workload", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}}},
				},
			}
			live := pod.DeepCopy()
			live.UID, live.Spec.NodeName = tc.uid, tc.node
			if tc.label != "" {
				live.Labels = map[string]string{hamiMonitorNodeLabel: tc.label}
			}
			client := fake.NewClientset(live)
			if tc.denyPatch {
				client.PrependReactor("patch", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("test denied"))
				})
			}
			manager := managerWithPods(t, pod)
			manager.clientset, manager.nodeName = client, "node-a"
			manager.containerHookPath, manager.hostHookPath = t.TempDir(), t.TempDir()
			claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}})
			cacheDirectory := filepath.Join(manager.containerHookPath, "vgpu", "containers", "pod-uid_workload")
			require.NoError(t, os.MkdirAll(cacheDirectory, 0755))
			sentinel := filepath.Join(cacheDirectory, "existing.cache")
			require.NoError(t, os.WriteFile(sentinel, []byte("keep existing cache"), 0600))
			edits, err := manager.GetCDIContainerEdits(context.Background(), claim, nil)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, edits)
				data, readErr := os.ReadFile(sentinel)
				require.NoError(t, readErr)
				require.Equal(t, "keep existing cache", string(data))
			} else {
				require.NoError(t, err)
				require.NotNil(t, edits)
				got, getErr := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				require.NoError(t, getErr)
				require.Equal(t, "node-a", got.Labels[hamiMonitorNodeLabel])
			}
			// The informer-owned Pod was not modified in place.
			require.Nil(t, pod.Labels)
		})
	}
}

func TestGetCDIContainerEditsLabelsTemplateInitPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid"},
		Spec: corev1.PodSpec{NodeName: "node-a",
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: ptr.To("gpu-template")}},
			InitContainers: []corev1.Container{{Name: "gpu-init", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}}},
		},
		Status: corev1.PodStatus{ResourceClaimStatuses: []corev1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: ptr.To("generated-claim")}}},
	}
	client := fake.NewClientset(pod.DeepCopy())
	manager := managerWithPods(t, pod)
	manager.clientset, manager.nodeName = client, "node-a"
	manager.containerHookPath = t.TempDir()
	claim := newResourceClaim("generated-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}})
	edits, err := manager.GetCDIContainerEdits(context.Background(), claim, nil)
	require.NoError(t, err)
	require.Contains(t, edits.Env, "CUDA_DEVICE_MEMORY_SHARED_CACHE="+filepath.Join(manager.containerHookPath, "vgpu", "containers", "pod-uid_gpu-init", "claim-uid.cache"))
	got, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "node-a", got.Labels[hamiMonitorNodeLabel])
}

func TestGetCDIContainerEditsMonitorLabelRetriesConcurrentMetadataChange(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod", UID: "pod-uid", ResourceVersion: "1", Labels: map[string]string{"app": "workload"}},
		Spec: corev1.PodSpec{NodeName: "node-a",
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: ptr.To("gpu-claim")}},
			Containers:     []corev1.Container{{Name: "workload", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}}},
		},
	}
	client := fake.NewClientset(pod.DeepCopy())
	conflicted := false
	client.PrependReactor("patch", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		var patch struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &patch))
		require.Equal(t, pod.UID, patch.Metadata.UID)
		if !conflicted {
			require.Equal(t, "1", patch.Metadata.ResourceVersion)
			conflicted = true
			changed := pod.DeepCopy()
			changed.ResourceVersion = "2"
			changed.Labels["concurrent"] = "preserved"
			require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), changed, pod.Namespace))
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("metadata changed"))
		}
		require.Equal(t, "2", patch.Metadata.ResourceVersion)
		return false, nil, nil
	})
	manager := managerWithPods(t, pod)
	manager.clientset, manager.nodeName = client, "node-a"
	manager.containerHookPath = t.TempDir()
	claim := newResourceClaim("gpu-claim", []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod.Name, UID: pod.UID}})
	edits, err := manager.GetCDIContainerEdits(context.Background(), claim, nil)
	require.NoError(t, err)
	require.NotNil(t, edits)
	got, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"app": "workload", "concurrent": "preserved", hamiMonitorNodeLabel: "node-a"}, got.Labels)
}

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
					NodeName:       "node-a",
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
			manager := &HAMiCoreManager{podLister: corelisters.NewPodLister(indexer), nodeName: "node-a"}
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

			gotPod, gotContainer, err := manager.resolveClaimToPod(context.Background(), claim)
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
			NodeName: "node-a",
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
			_, _, err := tt.manager.resolveClaimToPod(context.Background(), tt.claim)
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
	pod.Spec.NodeName = "node-a"
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

	// Use a live node-bound Pod for the monitor's metadata patch.
	livePod := pod.DeepCopy()
	livePod.Spec.NodeName = "node-a"
	manager.clientset = fake.NewClientset(livePod)
	manager.nodeName = "node-a"
	edits, err := manager.GetCDIContainerEdits(context.Background(), claim, devices)
	require.NoError(t, err)
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
	return &HAMiCoreManager{podLister: corelisters.NewPodLister(indexer), nodeName: "node-a"}
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
