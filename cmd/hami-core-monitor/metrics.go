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
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Project-HAMi/k8s-dra-driver/pkg/monitor"

	"k8s.io/klog/v2"
)

// ---------------------------------------------------------------------------
// Metric descriptors.
// Claim-level metrics carry: namespace, pod, container, claim_uid,
// vdevice_index, device_uuid.
// ---------------------------------------------------------------------------
var (
	hostGPUMemoryDesc = prometheus.NewDesc(
		"hami_host_gpu_memory_used_bytes",
		"GPU device memory usage in bytes",
		[]string{"device_index", "device_uuid", "device_type"}, nil,
	)

	hostGPUUtilizationDesc = prometheus.NewDesc(
		"hami_host_gpu_utilization_ratio",
		"GPU core utilization ratio (0-100)",
		[]string{"device_index", "device_uuid", "device_type"}, nil,
	)

	claimMemoryUsedDesc = prometheus.NewDesc(
		"hami_vgpu_memory_used_bytes",
		"vGPU device memory usage in bytes",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)

	claimMemoryLimitDesc = prometheus.NewDesc(
		"hami_vgpu_memory_limit_bytes",
		"vGPU device memory limit in bytes",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)

	claimDeviceMemoryDesc = prometheus.NewDesc(
		"hami_container_device_memory_bytes",
		"Container device memory usage breakdown in bytes",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid", "context_size", "module_size", "buffer_size", "offset"}, nil,
	)

	claimDeviceUtilizationDesc = prometheus.NewDesc(
		"hami_container_device_utilization_ratio",
		"Container device SM utilization ratio",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)

	claimDeviceMemoryContextDesc = prometheus.NewDesc(
		"hami_vgpu_memory_context_bytes",
		"Container device memory context size in bytes",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)

	claimDeviceMemoryModuleDesc = prometheus.NewDesc(
		"hami_vgpu_memory_module_bytes",
		"Container device memory module size in bytes",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)

	claimDeviceMemoryBufferDesc = prometheus.NewDesc(
		"hami_vgpu_memory_buffer_bytes",
		"Container device memory buffer size in bytes",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)

	claimLastKernelDesc = prometheus.NewDesc(
		"hami_container_last_kernel_elapsed_seconds",
		"Seconds since last kernel execution in container",
		[]string{"namespace", "pod", "container", "claim_uid", "vdevice_index", "device_uuid"}, nil,
	)
)

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

type collector struct {
	claimLister *monitor.ClaimLister
	mapper      *ClaimMapper
}

func newCollector(claimLister *monitor.ClaimLister, mapper *ClaimMapper) *collector {
	return &collector{
		claimLister: claimLister,
		mapper:      mapper,
	}
}

// Describe implements prometheus.Collector.
func (c collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- hostGPUMemoryDesc
	ch <- hostGPUUtilizationDesc
	ch <- claimMemoryUsedDesc
	ch <- claimMemoryLimitDesc
	ch <- claimDeviceMemoryDesc
	ch <- claimDeviceUtilizationDesc
	ch <- claimDeviceMemoryContextDesc
	ch <- claimDeviceMemoryModuleDesc
	ch <- claimDeviceMemoryBufferDesc
	ch <- claimLastKernelDesc
}

// Collect implements prometheus.Collector.
func (c collector) Collect(ch chan<- prometheus.Metric) {
	klog.V(4).InfoS("Starting metrics collection")

	if err := c.collectHostGPU(ch); err != nil {
		klog.ErrorS(err, "Failed to collect host GPU metrics")
	}
	if err := c.collectClaims(ch); err != nil {
		klog.ErrorS(err, "Failed to collect claim metrics")
	}

	klog.V(4).InfoS("Finished metrics collection")
}

// ---------------------------------------------------------------------------
// Host GPU metrics (via NVML).
// ---------------------------------------------------------------------------

func (c collector) collectHostGPU(ch chan<- prometheus.Metric) error {
	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return fmt.Errorf("nvml.Init: %s", nvml.ErrorString(ret))
	}
	defer nvml.Shutdown()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml.DeviceGetCount: %s", nvml.ErrorString(ret))
	}

	for i := 0; i < count; i++ {
		dev, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			klog.V(3).ErrorS(nil, "nvml.DeviceGetHandleByIndex", "index", i, "error", nvml.ErrorString(ret))
			continue
		}
		if err := c.collectHostDevice(ch, dev, i); err != nil {
			klog.V(3).ErrorS(err, "Failed to collect metrics for host GPU", "index", i)
		}
	}
	return nil
}

func (c collector) collectHostDevice(ch chan<- prometheus.Metric, dev nvml.Device, index int) error {
	uuid, ret := dev.GetUUID()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml.GetUUID: %s", nvml.ErrorString(ret))
	}

	name, ret := dev.GetName()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml.GetName: %s", nvml.ErrorString(ret))
	}
	deviceType := "NVIDIA-" + name
	idxStr := fmt.Sprint(index)

	// Memory.
	mem, ret := dev.GetMemoryInfo()
	if ret == nvml.ERROR_NOT_SUPPORTED {
		klog.V(3).InfoS("Memory metrics not supported for device, skipping", "index", index)
	} else if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml.GetMemoryInfo: %s", nvml.ErrorString(ret))
	} else {
		ch <- prometheus.MustNewConstMetric(hostGPUMemoryDesc, prometheus.GaugeValue, float64(mem.Used), idxStr, uuid, deviceType)
	}

	// Utilization.
	util, ret := dev.GetUtilizationRates()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("nvml.GetUtilizationRates: %s", nvml.ErrorString(ret))
	}
	ch <- prometheus.MustNewConstMetric(hostGPUUtilizationDesc, prometheus.GaugeValue, float64(util.Gpu), idxStr, uuid, deviceType)

	return nil
}

// ---------------------------------------------------------------------------
// Claim-level metrics (via mmaped shared memory caches).
// ---------------------------------------------------------------------------

func (c collector) collectClaims(ch chan<- prometheus.Metric) error {
	claims := c.claimLister.ListClaims()
	nowSec := time.Now().Unix()

	for _, claim := range claims {
		if claim.Info == nil {
			continue
		}
		if err := c.collectClaim(ch, claim, nowSec); err != nil {
			klog.V(3).ErrorS(err, "Failed to collect metrics for claim", "claimUID", claim.ClaimUID)
		}
	}
	return nil
}

func (c collector) collectClaim(ch chan<- prometheus.Metric, claim *monitor.ClaimUsage, nowSec int64) error {
	info := claim.Info
	baseLabels := resolve(claim.ClaimUID, c.mapper)

	for i := 0; i < info.DeviceNum(); i++ {
		uuid := info.DeviceUUID(i)
		if len(uuid) < 40 {
			klog.V(5).InfoS("Skipping device with invalid UUID length", "claim", claim.ClaimUID, "index", i, "len", len(uuid))
			continue
		}
		uuid = uuid[:40]
		if !utf8.ValidString(uuid) {
			klog.V(5).InfoS("Skipping device with invalid UTF-8 UUID; cache may not be initialised yet", "claim", claim.ClaimUID, "index", i)
			continue
		}

		labels := append(baseLabels, fmt.Sprint(i), uuid)

		// Memory used and limit.
		memTotal := info.DeviceMemoryTotal(i)
		memLimit := info.DeviceMemoryLimit(i)
		ch <- prometheus.MustNewConstMetric(claimMemoryUsedDesc, prometheus.GaugeValue, float64(memTotal), labels...)
		ch <- prometheus.MustNewConstMetric(claimMemoryLimitDesc, prometheus.GaugeValue, float64(memLimit), labels...)

		// Breakdown.
		memCtx := info.DeviceMemoryContextSize(i)
		memMod := info.DeviceMemoryModuleSize(i)
		memBuf := info.DeviceMemoryBufferSize(i)
		memOffset := memTotal - memCtx - memMod - memBuf
		breakdownLabels := append(labels, fmt.Sprint(memCtx), fmt.Sprint(memMod), fmt.Sprint(memBuf), fmt.Sprint(memOffset))
		ch <- prometheus.MustNewConstMetric(claimDeviceMemoryDesc, prometheus.GaugeValue, float64(memTotal), breakdownLabels...)

		// Context / module / buffer sub-metrics.
		ch <- prometheus.MustNewConstMetric(claimDeviceMemoryContextDesc, prometheus.GaugeValue, float64(memCtx), labels...)
		ch <- prometheus.MustNewConstMetric(claimDeviceMemoryModuleDesc, prometheus.GaugeValue, float64(memMod), labels...)
		ch <- prometheus.MustNewConstMetric(claimDeviceMemoryBufferDesc, prometheus.GaugeValue, float64(memBuf), labels...)

		// SM utilization.
		smUtil := info.DeviceSmUtil(i)
		ch <- prometheus.MustNewConstMetric(claimDeviceUtilizationDesc, prometheus.GaugeValue, float64(smUtil), labels...)

		// Last kernel time.
		lastKernelTime := info.LastKernelTime()
		if lastKernelTime > 0 {
			lastSec := max(nowSec-lastKernelTime, 0)
			ch <- prometheus.MustNewConstMetric(claimLastKernelDesc, prometheus.GaugeValue, float64(lastSec), labels...)
		}
	}
	return nil
}
