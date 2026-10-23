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
	"math"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const deviceMemoryAnnotation = "hami.io/default-device-memory"

type deviceMemoryBudget struct {
	parsed resource.Quantity
	bytes  uint64
}

func normalizeDeviceMemory(raw, source string) (deviceMemoryBudget, error) {
	// ParseQuantity narrows decimal exponents to int32 after parsing them.
	if i := strings.IndexAny(raw, "eE"); i >= 0 && i+1 < len(raw) && raw[i:] != "Ei" {
		if _, err := strconv.ParseInt(raw[i+1:], 10, 32); err != nil {
			return deviceMemoryBudget{}, fmt.Errorf("device memory %q: invalid Quantity exponent in %q: %w", source, raw, err)
		}
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return deviceMemoryBudget{}, fmt.Errorf("device memory %q: invalid Quantity %q: %w", source, raw, err)
	}
	// Leave room for rounding before converting to bytes. This also
	// rejects BinarySI inputs that ParseQuantity saturates at MaxInt64.
	maximum := resource.NewQuantity(math.MaxInt64/bytesPerMiB*bytesPerMiB, resource.BinarySI)
	if q.Sign() < 0 || q.Cmp(*maximum) > 0 {
		return deviceMemoryBudget{}, fmt.Errorf("device memory %q: Quantity %q must be between 0 and %s", source, raw, maximum.String())
	}
	bytes := (q.Value() + (bytesPerMiB - 1)) / bytesPerMiB * bytesPerMiB
	return deviceMemoryBudget{parsed: q, bytes: uint64(bytes)}, nil
}

// loadDeviceMemoryBudget resolves the fallback budget before device discovery.
func (c *Config) loadDeviceMemoryBudget(ctx context.Context, enabled bool) error {
	if !enabled {
		return nil
	}

	raw := c.flags.preConfiguredDeviceMemory
	source := "default"
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	node, nodeErr := c.clientsets.Core.CoreV1().Nodes().Get(requestCtx, c.flags.nodeName, metav1.GetOptions{})
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if nodeErr != nil {
		klog.Warningf("Could not read device memory annotation %q for node %q, using the default budget: %v", deviceMemoryAnnotation, c.flags.nodeName, nodeErr)
	} else if value, exists := node.Annotations[deviceMemoryAnnotation]; exists {
		raw = value
		source = fmt.Sprintf("node %q annotation %s", node.Name, deviceMemoryAnnotation)
	}

	budget, err := normalizeDeviceMemory(raw, source)
	if err != nil {
		return err
	}
	c.deviceMemoryBytes = budget.bytes
	effective := resource.NewQuantity(int64(budget.bytes), resource.BinarySI)
	if budget.parsed.Cmp(*effective) != 0 {
		klog.Warningf("Device memory rounded up: source=%s requested=%s effective=%s", source, raw, effective)
	} else {
		klog.InfoS("Device memory budget selected", "source", source, "requested", raw, "effective", effective.String())
	}
	return nil
}
