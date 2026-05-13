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
	"fmt"
	"strings"
	"time"

	"github.com/Project-HAMi/k8s-dra-driver/pkg/monitor"

	"k8s.io/klog/v2"
)

// utilizationPerDevice tracks how many processes with recent kernels are on a
// given physical GPU for a given priority class.
type utilizationPerDevice struct {
	count uint64
}

// watchAndFeedback runs a ticker-driven loop that periodically rescans the
// claim cache directory and applies the soft-QoS feedback rules.
func watchAndFeedback(ctx context.Context, lister *monitor.ClaimLister, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := lister.Update(); err != nil {
				klog.V(3).ErrorS(err, "Failed to update claim lister")
			}
			observe(lister)
		}
	}
}

// observe evaluates every active claim, decrements the recent-kernel counters,
// and mutates utilizationSwitch / recentKernel based on GPU-level contention.
func observe(lister *monitor.ClaimLister) {
	claims := lister.ListClaims()
	if len(claims) == 0 {
		return
	}

	// Aggregate active processes per (short-UUID, priority).
	utSwitchOn := make(map[string]utilizationPerDevice)

	for _, c := range claims {
		if c.Info == nil {
			continue
		}
		recentKernel := c.Info.GetRecentKernel()
		if recentKernel > 0 {
			recentKernel--
			for i := 0; i < c.Info.DeviceNum(); i++ {
				if !c.Info.IsValidUUID(i) {
					continue
				}
				uuid := strings.Split(c.Info.DeviceUUID(i), "-")[0]
				key := fmt.Sprintf("%s_%d", uuid, c.Info.GetPriority())
				utSwitchOn[key] = utilizationPerDevice{count: utSwitchOn[key].count + 1}
			}
		}
		c.Info.SetRecentKernel(recentKernel)
	}

	// Second pass: set blocking / priority flags.
	for _, c := range claims {
		if c.Info == nil {
			continue
		}
		if checkBlocking(utSwitchOn, c.Info.GetPriority(), c.Info) {
			c.Info.SetRecentKernel(-1)
		} else if c.Info.GetRecentKernel() < 0 {
			c.Info.SetRecentKernel(0)
		}
		if checkPriority(utSwitchOn, c.Info.GetPriority(), c.Info) {
			c.Info.SetUtilizationSwitch(1)
		} else {
			c.Info.SetUtilizationSwitch(0)
		}
	}
}

// checkBlocking returns true when another process with the same or higher
// priority has a recent kernel on any of the claim's devices.
func checkBlocking(utSwitchOn map[string]utilizationPerDevice, priority int, info monitor.UsageInfo) bool {
	for i := 0; i < info.DeviceNum(); i++ {
		if !info.IsValidUUID(i) {
			continue
		}
		uuid := strings.Split(info.DeviceUUID(i), "-")[0]
		for p := 0; p <= priority; p++ {
			key := fmt.Sprintf("%s_%d", uuid, p)
			if val, ok := utSwitchOn[key]; ok && val.count > 1 {
				return true
			}
		}
	}
	return false
}

// checkPriority returns true when any process with the same or higher priority
// is active on any of the claim's devices.
func checkPriority(utSwitchOn map[string]utilizationPerDevice, priority int, info monitor.UsageInfo) bool {
	for i := 0; i < info.DeviceNum(); i++ {
		if !info.IsValidUUID(i) {
			continue
		}
		uuid := strings.Split(info.DeviceUUID(i), "-")[0]
		for p := 0; p <= priority; p++ {
			key := fmt.Sprintf("%s_%d", uuid, p)
			if val, ok := utSwitchOn[key]; ok && val.count > 0 {
				return true
			}
		}
	}
	return false
}
