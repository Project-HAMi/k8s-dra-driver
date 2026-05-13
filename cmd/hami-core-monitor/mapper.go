/*
 * Copyright 2025 The HAMi Authors.
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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corev1informer "k8s.io/client-go/informers/core/v1"
	resourcev1informer "k8s.io/client-go/informers/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	resyncPeriod      = 30 * time.Minute
	hamiDeviceClassName = "hami-core-gpu.project-hami.io"
)

// PodRef holds pod metadata for a ResourceClaim.
type PodRef struct {
	Namespace string
	PodName   string
	Container string // empty if none, "_multiple" if >1
}

// ClaimMapper maintains an eventually-consistent mapping from ResourceClaim UID
// to the pod (and container) consuming it, driven by shared informers.
type ClaimMapper struct {
	nodeName      string
	podInformer   corev1informer.PodInformer
	claimInformer resourcev1informer.ResourceClaimInformer

	mu      sync.RWMutex
	mapping map[string]PodRef // claimUID -> pod metadata
}

// NewClaimMapper creates a mapper and wires shared informers.
func NewClaimMapper(client kubernetes.Interface, nodeName string) *ClaimMapper {
	// Pod informer scoped to this node via field selector.
	podFactory := informers.NewSharedInformerFactoryWithOptions(
		client,
		resyncPeriod,
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.FieldSelector = "spec.nodeName=" + nodeName
		}),
	)

	// Claim informer watches the whole cluster.
	claimFactory := informers.NewSharedInformerFactory(client, resyncPeriod)

	return &ClaimMapper{
		nodeName:      nodeName,
		podInformer:   podFactory.Core().V1().Pods(),
		claimInformer: claimFactory.Resource().V1().ResourceClaims(),
		mapping:       make(map[string]PodRef),
	}
}

// Start runs both informers and blocks until ctx is cancelled.
func (m *ClaimMapper) Start(ctx context.Context) {
	// Debounced full rebuild channel – fed by the periodic ticker only.
	rebuildCh := make(chan struct{}, 1)

	// Wire event handlers for targeted partial updates.
	_, _ = m.podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.upsertPod(obj.(*corev1.Pod)) },
		UpdateFunc: func(_, newObj any) { m.upsertPod(newObj.(*corev1.Pod)) },
		DeleteFunc: m.removePod,
	})
	_, _ = m.claimInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { m.upsertClaim(obj.(*resourcev1.ResourceClaim)) },
		UpdateFunc: func(_, newObj any) { m.upsertClaim(newObj.(*resourcev1.ResourceClaim)) },
		DeleteFunc: m.removeClaim,
	})

	stopPod := make(chan struct{})
	stopClaim := make(chan struct{})
	go m.podInformer.Informer().Run(stopPod)
	go m.claimInformer.Informer().Run(stopClaim)
	defer close(stopPod)
	defer close(stopClaim)

	if !cache.WaitForCacheSync(ctx.Done(), m.podInformer.Informer().HasSynced, m.claimInformer.Informer().HasSynced) {
		klog.V(2).InfoS("ClaimMapper cache sync cancelled")
		return
	}

	// Initial full rebuild before any events arrive.
	m.fullRebuild()

	// Periodic safety-net full rebuild.
	ticker := time.NewTicker(resyncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case rebuildCh <- struct{}{}:
			default:
			}
		case <-rebuildCh:
			m.fullRebuild()
		}
	}
}

// Lookup returns pod metadata for a given claim UID.
func (m *ClaimMapper) Lookup(claimUID string) (PodRef, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ref, ok := m.mapping[claimUID]
	return ref, ok
}

// ---------------------------------------------------------------------------
// Partial (targeted) updates driven by informer events.
// ---------------------------------------------------------------------------

// upsertPod adds or updates the mapping entries for every HAMi claim
// referenced by the given pod.  Called on Pod Add/Update.
func (m *ClaimMapper) upsertPod(pod *corev1.Pod) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, status := range pod.Status.ResourceClaimStatuses {
		if status.ResourceClaimName == nil || *status.ResourceClaimName == "" {
			continue
		}
		claim, err := m.claimInformer.Lister().ResourceClaims(pod.Namespace).Get(*status.ResourceClaimName)
		if err != nil {
			// Claim not in cache yet — will be fixed when the claim event arrives.
			continue
		}
		if !isHamiClaim(claim) {
			continue
		}
		m.mapping[string(claim.UID)] = m.makePodRef(pod, status.Name, *status.ResourceClaimName)
	}
}

// removePod deletes all mapping entries that belong to the given pod.
// Called on Pod Delete.
func (m *ClaimMapper) removePod(obj any) {
	var pod *corev1.Pod
	switch t := obj.(type) {
	case *corev1.Pod:
		pod = t
	case cache.DeletedFinalStateUnknown:
		pod = t.Obj.(*corev1.Pod)
	default:
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for uid, ref := range m.mapping {
		if ref.Namespace == pod.Namespace && ref.PodName == pod.Name {
			delete(m.mapping, uid)
		}
	}
}

// upsertClaim resolves the given claim back to a pod on this node and inserts
// the mapping entry.  Called on ResourceClaim Add/Update.
func (m *ClaimMapper) upsertClaim(claim *resourcev1.ResourceClaim) {
	if !isHamiClaim(claim) {
		return
	}
	// Find the pod on this node that references this claim.
	pods, err := m.podInformer.Lister().List(labels.Everything())
	if err != nil {
		klog.V(3).ErrorS(err, "ClaimMapper: failed to list pods from cache")
		return
	}
	for _, pod := range pods {
		for _, status := range pod.Status.ResourceClaimStatuses {
			if status.ResourceClaimName == nil || *status.ResourceClaimName == "" {
				continue
			}
			if pod.Namespace == claim.Namespace && *status.ResourceClaimName == claim.Name {
				m.mu.Lock()
				m.mapping[string(claim.UID)] = m.makePodRef(pod, status.Name, claim.Name)
				m.mu.Unlock()
				return // DRA: a claim is bound to at most one pod.
			}
		}
	}
}

// removeClaim deletes the mapping entry for the given claim.
// Called on ResourceClaim Delete.
func (m *ClaimMapper) removeClaim(obj any) {
	var claim *resourcev1.ResourceClaim
	switch t := obj.(type) {
	case *resourcev1.ResourceClaim:
		claim = t
	case cache.DeletedFinalStateUnknown:
		claim = t.Obj.(*resourcev1.ResourceClaim)
	default:
		return
	}

	m.mu.Lock()
	delete(m.mapping, string(claim.UID))
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Full rebuild (safety net, run once at startup and then periodically).
// ---------------------------------------------------------------------------

func (m *ClaimMapper) fullRebuild() {
	pods, err := m.podInformer.Lister().List(labels.Everything())
	if err != nil {
		klog.V(3).ErrorS(err, "ClaimMapper: full rebuild failed listing pods")
		return
	}

	newMapping := make(map[string]PodRef, len(pods))
	for _, pod := range pods {
		for _, status := range pod.Status.ResourceClaimStatuses {
			if status.ResourceClaimName == nil || *status.ResourceClaimName == "" {
				continue
			}
			claim, err := m.claimInformer.Lister().ResourceClaims(pod.Namespace).Get(*status.ResourceClaimName)
			if err != nil {
				continue
			}
			if !isHamiClaim(claim) {
				continue
			}
			newMapping[string(claim.UID)] = m.makePodRef(pod, status.Name, *status.ResourceClaimName)
		}
	}

	m.mu.Lock()
	m.mapping = newMapping
	m.mu.Unlock()

	klog.V(4).InfoS("ClaimMapper full rebuild", "pods", len(pods), "claims", len(newMapping))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (m *ClaimMapper) makePodRef(pod *corev1.Pod, localClaimName, claimName string) PodRef {
	containers := containersUsingClaim(pod, localClaimName)
	containerName := ""
	switch len(containers) {
	case 1:
		containerName = containers[0]
	case 0:
		containerName = ""
	default:
		containerName = "_multiple"
	}
	return PodRef{
		Namespace: pod.Namespace,
		PodName:   pod.Name,
		Container: containerName,
	}
}

// isHamiClaim returns true if the claim is for the HAMi device class.
func isHamiClaim(claim *resourcev1.ResourceClaim) bool {
	for _, req := range claim.Spec.Devices.Requests {
		if req.Exactly != nil && req.Exactly.DeviceClassName == hamiDeviceClassName {
			return true
		}
		for _, sub := range req.FirstAvailable {
			if sub.DeviceClassName == hamiDeviceClassName {
				return true
			}
		}
	}
	return false
}

func containersUsingClaim(pod *corev1.Pod, localClaimName string) []string {
	var names []string
	for _, c := range pod.Spec.Containers {
		for _, rc := range c.Resources.Claims {
			if rc.Name == localClaimName {
				names = append(names, c.Name)
				break
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, rc := range c.Resources.Claims {
			if rc.Name == localClaimName {
				names = append(names, c.Name)
				break
			}
		}
	}
	return names
}

var unknownPodRef = PodRef{Namespace: "unknown", PodName: "unknown", Container: "unknown"}

func resolve(claimUID string, mapper *ClaimMapper) []string {
	if mapper == nil {
		return []string{unknownPodRef.Namespace, unknownPodRef.PodName, unknownPodRef.Container, claimUID}
	}
	ref, ok := mapper.Lookup(claimUID)
	if !ok {
		return []string{unknownPodRef.Namespace, unknownPodRef.PodName, unknownPodRef.Container, claimUID}
	}
	return []string{ref.Namespace, ref.PodName, ref.Container, claimUID}
}
