/*
 * Copyright (c) 2022-2025, NVIDIA CORPORATION.  All rights reserved.
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
	"slices"

	resourceapi "k8s.io/api/resource/v1"

	// "github.com/NVIDIA/k8s-dra-driver-gpu/pkg/featuregates"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/featuregates"
)

// The device name is the canonical device name announced by us a DRA
// ResourceSlice). It must be a (node-local) unambiguous device identifier. It's
// exposed to users in error messages. It's played back to us upon a
// NodePrepareResources request, which is when we look it up in the
// `AllocatableDevices` map. Conceptually, this the same as
// kubeletplugin.Device.DeviceName (documented with 'DeviceName identifies the
// device inside that pool').
type DeviceName = string

type AllocatableDevices map[DeviceName]*AllocatableDevice

// AllocatableDevice represents an individual device that can be allocated.
type AllocatableDevice struct {
	HAMiGpu    *HAMiGpuInfo
	Gpu        *GpuInfo
	MigDynamic *MigSpec
	MigStatic  *MigDeviceInfo
	Vfio       *VfioDeviceInfo
}

type AllocatableDeviceList []*AllocatableDevice

func (d AllocatableDevice) Type() string {
	if d.HAMiGpu != nil {
		return HAMiGpuDeviceType
	}
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.MigDynamic != nil {
		return MigDynamicDeviceType
	}
	if d.MigStatic != nil {
		return MigStaticDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

func (d AllocatableDevice) IsStaticOrDynMigDevice() bool {
	switch d.Type() {
	case MigStaticDeviceType, MigDynamicDeviceType:
		return true
	default:
		return false
	}
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case HAMiGpuDeviceType:
		return d.HAMiGpu.CanonicalName()
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case MigStaticDeviceType:
		return d.MigStatic.CanonicalName()
	case MigDynamicDeviceType:
		return d.MigDynamic.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	switch d.Type() {
	case HAMiGpuDeviceType:
		return d.HAMiGpu.GetDevice()
	case GpuDeviceType:
		return d.Gpu.GetDevice()
	case MigStaticDeviceType:
		return d.MigStatic.GetDevice()
	case MigDynamicDeviceType:
		panic("GetDevice() must currently not be called for MigDynamicDeviceType")
	case VfioDeviceType:
		return d.Vfio.GetDevice()
	}
	panic("unexpected type for AllocatableDevice")
}

// UUID() is here for `AllocatableDevices` to implement the `UUIDProvider`
// interface. Conceptually, at least since introduction of DynamicMIG, some
// allocatable devices are abstract devices that do not have a UUID before
// actualization -- hence, the idea of `AllocatableDevices` implementing
// UUIDProvider is brittle.
func (d AllocatableDevice) UUID() string {
	if d.HAMiGpu != nil {
		return d.HAMiGpu.UUID
	}
	if d.Gpu != nil {
		return d.Gpu.UUID
	}
	if d.MigStatic != nil {
		return d.MigStatic.UUID
	}
	if d.MigDynamic != nil {
		// For now, the caller must sure to never call UUID() on such a device.
		// This method for now exists because when the DynamicMIG feature gate
		// is disabled, `AllocatableDevices` _can_ implement UUIDProvider; and
		// that feature is used throughout the code base. This needs
		// restructuring and cleanup.
		panic("unexpected UUID() call for AllocatableDevice of type MigDynamic")
	}
	if d.Vfio != nil {
		return d.Vfio.UUID
	}
	panic("unexpected type for AllocatableDevice")
}

func (d AllocatableDevices) getDevicesByGPUPCIBusID(pcieBusID string) AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		switch device.Type() {
		case HAMiGpuDeviceType:
			if device.HAMiGpu.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case GpuDeviceType:
			if device.Gpu.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case MigStaticDeviceType:
			if device.MigStatic.parent.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case MigDynamicDeviceType:
			if device.MigDynamic.Parent.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		case VfioDeviceType:
			if device.Vfio.pcieBusID == pcieBusID {
				devices = append(devices, device)
			}
		}
	}
	return devices
}

func (d AllocatableDevices) GetGPUByPCIeBusID(pcieBusID string) *AllocatableDevice {
	// TODO: Check HAMiGpuDeviceType?
	for _, device := range d {
		if device.Type() != GpuDeviceType {
			continue
		}
		if device.Gpu.pcieBusID == pcieBusID {
			return device
		}
	}
	return nil
}

func (d AllocatableDevices) GetGPUs() AllocatableDeviceList {
	// TODO: Check HAMiGpuDeviceType?
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetVfioDevices() AllocatableDeviceList {
	var devices AllocatableDeviceList
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// Required for implementing UUIDProvider. Meant to return (only) full GPU UUIDs.
func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, dev := range d {
		if dev.Type() == HAMiGpuDeviceType {
			uuids = append(uuids, dev.UUID())
		}
		if dev.Type() == GpuDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	slices.Sort(uuids)
	return uuids
}

// Required for implementing UUIDProvider. Meant to return MIG device UUIDs.
// Must not be called when the DynamicMIG featuregate is enabled.
func (d AllocatableDevices) MigDeviceUUIDs() []string {
	if featuregates.Enabled(featuregates.DynamicMIG) {
		panic("MigDeviceUUIDs() unexpectedly called (DynamicMIG is enabled)")
	}
	var uuids []string
	for _, dev := range d {
		if dev.Type() == MigStaticDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			uuids = append(uuids, device.Vfio.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

// Required for implementing UUIDProvider. Meant to return full GPU UUIDs and
// MIG device UUIDs. Must not be used when the DynamicMIG featuregate is
// enabled. Unsure what it's supposed to return for VFIO devices.
func (d AllocatableDevices) UUIDs() []string {
	var uuids []string
	for _, dev := range d {
		uuids = append(uuids, dev.UUID())
	}
	slices.Sort(uuids)
	return uuids
}

// TODO: This needs a code comment, clarifying the complexity across device
// types. This function is tied to PassthroughSuppert and hence for now
// guaranteed to not be exercised when DynamicMIG is enabled.
func (d AllocatableDevices) RemoveSiblingDevices(device *AllocatableDevice) {
	var pciBusID string
	switch device.Type() {
	case GpuDeviceType:
		pciBusID = device.Gpu.pcieBusID
	case VfioDeviceType:
		pciBusID = device.Vfio.pcieBusID
	case MigStaticDeviceType:
		// TODO: Implement once/if static MIG is supported in the context of
		// PassthroughSupport.
		return
	case MigDynamicDeviceType:
		// TODO: Implement once/if dynamic MIG is supported in the context of
		// PassthroughSupport.
		return
	}

	siblings := d.getDevicesByGPUPCIBusID(pciBusID)
	for _, sibling := range siblings {
		if sibling.Type() == device.Type() {
			continue
		}
		switch sibling.Type() {
		case GpuDeviceType:
			delete(d, sibling.Gpu.CanonicalName())
		case VfioDeviceType:
			delete(d, sibling.Vfio.CanonicalName())
		case MigStaticDeviceType:
			// TODO
			continue
		case MigDynamicDeviceType:
			// TODO
			continue
		}
	}
}

func (d *AllocatableDevice) IsHealthy() bool {
	switch d.Type() {
	case HAMiGpuDeviceType:
		return d.HAMiGpu.health == Healthy
	case GpuDeviceType:
		return d.Gpu.health == Healthy
	case MigStaticDeviceType:
		// TODO: review -- what about the parent?
		return d.MigStatic.health == Healthy
	case MigDynamicDeviceType:
		// TODOMIG: For now, pretend health -- this device maybe hasn't
		// manifested yet. Or has it? We could adopt the health status of the
		// parent, but that's also not meaningful I think.
		return true
	}
	panic("unexpected type for AllocatableDevice")
}
