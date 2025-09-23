package main

import (
	"slices"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type PreparedDeviceList []PreparedDevice
type PreparedDevices []*PreparedDeviceGroup

type PreparedDevice struct {
	Gpu *PreparedGpu       `json:"gpu"`
	HAMiGpu *PreparedHAMiGpu   `json:"hami-gpu"`
	Mig *PreparedMigDevice `json:"mig"`
}

type PreparedGpu struct {
	Info   *GpuInfo              `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}

type PreparedHAMiGpu struct {
	Info   *HAMiGpuInfo          `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}


type PreparedMigDevice struct {
	Info   *MigDeviceInfo        `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}

type PreparedDeviceGroup struct {
	Devices     PreparedDeviceList `json:"devices"`
	ConfigState DeviceConfigState  `json:"configState"`
}

func (d PreparedDevice) Type() string {
	if d.Gpu != nil {
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

func (d *PreparedDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.Info.CanonicalName()
	case HAMiGpuDeviceType:
		return d.HAMiGpu.Info.CanonicalName()
	case MigDeviceType:
		return d.Mig.Info.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *PreparedDevice) CanonicalIndex() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.Info.CanonicalIndex()
	case HAMiGpuDeviceType:
		return d.HAMiGpu.Info.CanonicalIndex()
	case MigDeviceType:
		return d.Mig.Info.CanonicalIndex()
	}
	panic("unexpected type for AllocatableDevice")
}

func (l PreparedDeviceList) Gpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) HAMiGpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == HAMiGpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) MigDevices() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == MigDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d PreparedDevices) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, group := range d {
		devices = append(devices, group.GetDevices()...)
	}
	return devices
}

func (g *PreparedDeviceGroup) GetDevices() []kubeletplugin.Device {
	var devices []kubeletplugin.Device
	for _, device := range g.Devices {
		switch device.Type() {
		case GpuDeviceType:
			devices = append(devices, *device.Gpu.Device)
		case HAMiGpuDeviceType:
			devices = append(devices, *device.HAMiGpu.Device)
		case MigDeviceType:
			devices = append(devices, *device.Mig.Device)
		}
	}
	return devices
}

func (l PreparedDeviceList) UUIDs() []string {
	uuids := append(l.GpuUUIDs(), l.MigDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) UUIDs() []string {
	uuids := append(g.GpuUUIDs(), g.MigDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (d PreparedDevices) UUIDs() []string {
	uuids := append(d.GpuUUIDs(), d.MigDeviceUUIDs()...)
	slices.Sort(uuids)
	return uuids
}

func (l PreparedDeviceList) GpuUUIDs() []string {
	var uuids []string
	for _, device := range l.Gpus() {
		uuids = append(uuids, device.Gpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (l PreparedDeviceList) HAMiGpuUUIDs() []string {
	var uuids []string
	for _, device := range l.HAMiGpus() {
		uuids = append(uuids, device.HAMiGpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) GpuUUIDs() []string {
	return g.Devices.Gpus().UUIDs()
}

func (g *PreparedDeviceGroup) HAMIGpuUUIDs() []string {
	return g.Devices.HAMiGpus().UUIDs()
}

func (d PreparedDevices) GpuUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.GpuUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

func (d PreparedDevices) HAMiGpuUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.HAMIGpuUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}

func (l PreparedDeviceList) MigDeviceUUIDs() []string {
	var uuids []string
	for _, device := range l.MigDevices() {
		uuids = append(uuids, device.Mig.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) MigDeviceUUIDs() []string {
	return g.Devices.MigDevices().UUIDs()
}

func (d PreparedDevices) MigDeviceUUIDs() []string {
	var uuids []string
	for _, group := range d {
		uuids = append(uuids, group.MigDeviceUUIDs()...)
	}
	slices.Sort(uuids)
	return uuids
}
