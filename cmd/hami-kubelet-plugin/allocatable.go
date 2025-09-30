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
	"slices"

	resourceapi "k8s.io/api/resource/v1"
)

type AllocatableDevices map[string]*AllocatableDevice

type AllocatableDevice struct {
	Gpu     *GpuInfo
	HAMiGpu *HAMiGpuInfo
	Mig     *MigDeviceInfo
}

func (d AllocatableDevice) Type() string {
	if d.HAMiGpu != nil {
		return GpuDeviceType
	}
	if d.HAMiGpu != nil {
		return HAMiGpuDeviceType
	}
	if d.Mig != nil {
		return MigDeviceType
	}
	return UnknownDeviceType
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case HAMiGpuDeviceType:
		return d.HAMiGpu.CanonicalName()
	case MigDeviceType:
		return d.Mig.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) CanonicalIndex() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.CanonicalIndex()
	case HAMiGpuDeviceType:
		return d.HAMiGpu.CanonicalIndex()
	case MigDeviceType:
		return d.Mig.CanonicalIndex()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.GetDevice()
	case HAMiGpuDeviceType:
		return d.HAMiGpu.GetDevice()
	case MigDeviceType:
		return d.Mig.GetDevice()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetSharedCounterSet() resourceapi.CounterSet {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.GetSharedCounterSet()
	case HAMiGpuDeviceType:
		return d.HAMiGpu.GetSharedCounterSet()
	case MigDeviceType:
		return d.Mig.GetSharedCounterSet()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			uuids = append(uuids, device.Gpu.UUID)
		}
		if device.Type() == HAMiGpuDeviceType {
			uuids = append(uuids, device.HAMiGpu.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) MigDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == MigDeviceType {
			uuids = append(uuids, device.Mig.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}
