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

const v1MaxDevices = 16

type v1DeviceMemory struct {
	contextSize uint64
	moduleSize  uint64
	bufferSize  uint64
	offset      uint64
	total       uint64
	unused      [3]uint64
}

type v1DeviceUtilization struct {
	decUtil uint64
	encUtil uint64
	smUtil  uint64
	unused  [3]uint64
}

type v1ShrregProcSlotT struct {
	pid         int32
	hostpid     int32
	used        [16]v1DeviceMemory
	monitorused [16]uint64
	deviceUtil  [16]v1DeviceUtilization
	status      int32
	unused      [3]uint64
}

type v1UUID struct {
	uuid [96]byte
}

type v1SemT struct {
	sem [32]byte
}

type v1SharedRegionT struct {
	initializedFlag int32
	majorVersion    int32
	minorVersion    int32
	smInitFlag      int32
	ownerPid        uint32
	sem             v1SemT
	num             uint64
	uuids           [16]v1UUID

	limit   [16]uint64
	smLimit [16]uint64
	procs   [1024]v1ShrregProcSlotT

	procnum           int32
	utilizationSwitch int32
	recentKernel      int32
	priority          int32
	lastKernelTime    int64
	unused            [4]uint64
}

type v1Spec struct {
	sr *v1SharedRegionT
}

func (s v1Spec) DeviceMax() int {
	return v1MaxDevices
}

func (s v1Spec) DeviceNum() int {
	return int(s.sr.num)
}

func (s v1Spec) DeviceMemoryContextSize(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs[:int(s.sr.procnum)] {
		v += p.used[idx].contextSize
	}
	return v
}

func (s v1Spec) DeviceMemoryModuleSize(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs[:int(s.sr.procnum)] {
		v += p.used[idx].moduleSize
	}
	return v
}

func (s v1Spec) DeviceMemoryBufferSize(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs[:int(s.sr.procnum)] {
		v += p.used[idx].bufferSize
	}
	return v
}

func (s v1Spec) DeviceMemoryOffset(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs[:int(s.sr.procnum)] {
		v += p.used[idx].offset
	}
	return v
}

func (s v1Spec) DeviceMemoryTotal(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs[:int(s.sr.procnum)] {
		v += p.used[idx].total
	}
	return v
}

func (s v1Spec) DeviceSmUtil(idx int) uint64 {
	var v uint64
	for _, p := range s.sr.procs[:int(s.sr.procnum)] {
		v += p.deviceUtil[idx].smUtil
	}
	return v
}

func (s v1Spec) SetDeviceSmLimit(l uint64) {
	var idx uint64
	for idx < s.sr.num {
		s.sr.smLimit[idx] = l
		idx++
	}
}

func (s v1Spec) IsValidUUID(idx int) bool {
	return s.sr.uuids[idx].uuid[0] != 0
}

func (s v1Spec) DeviceUUID(idx int) string {
	return string(s.sr.uuids[idx].uuid[:])
}

func (s v1Spec) DeviceMemoryLimit(idx int) uint64 {
	return s.sr.limit[idx]
}

func (s v1Spec) SetDeviceMemoryLimit(l uint64) {
	var idx uint64
	for idx < s.sr.num {
		s.sr.limit[idx] = l
		idx++
	}
}

func (s v1Spec) LastKernelTime() int64 {
	return s.sr.lastKernelTime
}

func castV1Spec(data []byte) UsageInfo {
	return v1Spec{
		sr: (*v1SharedRegionT)(unsafe.Pointer(&data[0])),
	}
}

func (s v1Spec) GetPriority() int {
	return int(s.sr.priority)
}

func (s v1Spec) GetRecentKernel() int32 {
	return s.sr.recentKernel
}

func (s v1Spec) SetRecentKernel(v int32) {
	s.sr.recentKernel = v
}

func (s v1Spec) GetUtilizationSwitch() int32 {
	return s.sr.utilizationSwitch
}

func (s v1Spec) SetUtilizationSwitch(v int32) {
	s.sr.utilizationSwitch = v
}
