/*
 * Copyright 2024-2025 The HAMi Authors.
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

package monitor

import "unsafe"

const v0MaxDevices = 16

type v0DeviceMemory struct {
	contextSize uint64
	moduleSize  uint64
	bufferSize  uint64
	offset      uint64
	total       uint64
}

type v0DeviceUtilization struct {
	decUtil uint64
	encUtil uint64
	smUtil  uint64
}

type v0ShrregProcSlotT struct {
	pid         int32
	hostpid     int32
	used        [16]v0DeviceMemory
	monitorused [16]uint64
	deviceUtil  [16]v0DeviceUtilization
	status      int32
}

type v0UUID struct {
	uuid [96]byte
}

type v0SemT struct {
	sem [32]byte
}

type v0SharedRegionT struct {
	initializedFlag int32
	smInitFlag      int32
	ownerPid        uint32
	sem             v0SemT
	num             uint64
	uuids           [16]v0UUID

	limit   [16]uint64
	smLimit [16]uint64
	procs   [1024]v0ShrregProcSlotT

	procnum           int32
	utilizationSwitch int32
	recentKernel      int32
	priority          int32
}

type v0Spec struct {
	sr *v0SharedRegionT
}

func (s v0Spec) DeviceMax() int {
	return v0MaxDevices
}

func (s v0Spec) DeviceNum() int {
	return int(s.sr.num)
}

func (s v0Spec) DeviceMemoryContextSize(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs {
		v += p.used[idx].contextSize
	}
	return v
}

func (s v0Spec) DeviceMemoryModuleSize(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs {
		v += p.used[idx].moduleSize
	}
	return v
}

func (s v0Spec) DeviceMemoryBufferSize(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs {
		v += p.used[idx].bufferSize
	}
	return v
}

func (s v0Spec) DeviceMemoryOffset(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs {
		v += p.used[idx].offset
	}
	return v
}

func (s v0Spec) DeviceMemoryTotal(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs {
		v += p.used[idx].total
	}
	return v
}

func (s v0Spec) DeviceSmUtil(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs {
		v += p.deviceUtil[idx].smUtil
	}
	return v
}

func (s v0Spec) SetDeviceSmLimit(l uint64) {
	var idx uint64
	for idx < s.sr.num {
		s.sr.smLimit[idx] = l
		idx++
	}
}

func (s v0Spec) IsValidUUID(idx int) bool {
	return s.sr.uuids[idx].uuid[0] != 0
}

func (s v0Spec) DeviceUUID(idx int) string {
	return string(s.sr.uuids[idx].uuid[:])
}

func (s v0Spec) DeviceMemoryLimit(idx int) uint64 {
	return s.sr.limit[idx]
}

func (s v0Spec) SetDeviceMemoryLimit(l uint64) {
	var idx uint64
	for idx < s.sr.num {
		s.sr.limit[idx] = l
		idx++
	}
}

func (s v0Spec) LastKernelTime() int64 {
	return 0
}

func castV0Spec(data []byte) UsageInfo {
	return v0Spec{
		sr: (*v0SharedRegionT)(unsafe.Pointer(&data[0])),
	}
}

func (s v0Spec) GetPriority() int {
	return int(s.sr.priority)
}

func (s v0Spec) GetRecentKernel() int32 {
	return s.sr.recentKernel
}

func (s v0Spec) SetRecentKernel(v int32) {
	s.sr.recentKernel = v
}

func (s v0Spec) GetUtilizationSwitch() int32 {
	return s.sr.utilizationSwitch
}

func (s v0Spec) SetUtilizationSwitch(v int32) {
	s.sr.utilizationSwitch = v
}
