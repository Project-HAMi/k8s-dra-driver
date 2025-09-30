
package main

const (
	GpuDeviceType     = "gpu"
	HAMiGpuDeviceType = "hami-gpu"
	MigDeviceType     = "mig"
	UnknownDeviceType = "unknown"
)

type UUIDProvider interface {
	UUIDs() []string
	GpuUUIDs() []string
	MigDeviceUUIDs() []string
}
