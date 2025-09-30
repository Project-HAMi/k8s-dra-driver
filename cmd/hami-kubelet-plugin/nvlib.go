
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
)

type deviceLib struct {
	nvdev.Interface
	nvmllib           nvml.Interface
	driverLibraryPath string
	devRoot           string
	nvidiaSMIPath     string
}

func newDeviceLib(driverRoot root) (*deviceLib, error) {
	driverLibraryPath, err := driverRoot.getDriverLibraryPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate driver libraries: %w", err)
	}

	nvidiaSMIPath, err := driverRoot.getNvidiaSMIPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate nvidia-smi: %w", err)
	}

	// We construct an NVML library specifying the path to libnvidia-ml.so.1
	// explicitly so that we don't have to rely on the library path.
	nvmllib := nvml.New(
		nvml.WithLibraryPath(driverLibraryPath),
	)
	d := deviceLib{
		Interface:         nvdev.New(nvmllib),
		nvmllib:           nvmllib,
		driverLibraryPath: driverLibraryPath,
		devRoot:           driverRoot.getDevRoot(),
		nvidiaSMIPath:     nvidiaSMIPath,
	}
	return &d, nil
}

// prependPathListEnvvar prepends a specified list of strings to a specified envvar and returns its value.
func prependPathListEnvvar(envvar string, prepend ...string) string {
	if len(prepend) == 0 {
		return os.Getenv(envvar)
	}
	current := filepath.SplitList(os.Getenv(envvar))
	return strings.Join(append(prepend, current...), string(filepath.ListSeparator))
}

// setOrOverrideEnvvar adds or updates an envar to the list of specified envvars and returns it.
func setOrOverrideEnvvar(envvars []string, key, value string) []string {
	var updated []string
	for _, envvar := range envvars {
		pair := strings.SplitN(envvar, "=", 2)
		if pair[0] == key {
			continue
		}
		updated = append(updated, envvar)
	}
	return append(updated, fmt.Sprintf("%s=%s", key, value))
}

func (l deviceLib) Init() error {
	ret := l.nvmllib.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error initializing NVML: %v", ret)
	}
	return nil
}

func (l deviceLib) alwaysShutdown() {
	ret := l.nvmllib.Shutdown()
	if ret != nvml.SUCCESS {
		klog.Warningf("error shutting down NVML: %v", ret)
	}
}

func (l deviceLib) enumerateAllPossibleDevices(config *Config) (AllocatableDevices, error) {
	alldevices := make(AllocatableDevices)

	gms, err := l.enumerateGpusDevicesForHAMiCore(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating GPU devices for HAMi-Core: %w", err)
	}
	for k, v := range gms {
		alldevices[k] = v
	}

	return alldevices, nil
}


// TODO: Merge this func into enumerateGpusAndMigDevices
func (l deviceLib) enumerateGpusDevicesForHAMiCore(config *Config) (AllocatableDevices, error) {
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	// splitCount := config.flags.hamiCoreDevSplitCount
	devices := make(AllocatableDevices)
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		// for idx := range splitCount {
		hamiGpuInfo := &HAMiGpuInfo{
			GpuInfo: GpuInfo{
				UUID:                  gpuInfo.UUID,
				minor:                 gpuInfo.minor,
				index:                 gpuInfo.index,
				migEnabled:            gpuInfo.migEnabled,
				memoryBytes:           gpuInfo.memoryBytes,
				productName:           gpuInfo.productName,
				brand:                 gpuInfo.brand,
				architecture:          gpuInfo.architecture,
				cudaComputeCapability: gpuInfo.cudaComputeCapability,
				driverVersion:         gpuInfo.driverVersion,
				cudaDriverVersion:     gpuInfo.cudaDriverVersion,
				pcieBusID:             gpuInfo.pcieBusID,
				pcieRootAttr:          gpuInfo.pcieRootAttr,
				migProfiles:           gpuInfo.migProfiles,
			},
			// TODO: A better mapping method should be applied
			// hamiSplitCounter: splitCount,
			// hamiIndex:        int(idx),
			// hamiSMLimit:      100 / splitCount,
			// hamiMemoryBytes:  gpuInfo.memoryBytes / uint64(splitCount),
		}
		deviceInfo := &AllocatableDevice{
			HAMiGpu: hamiGpuInfo,
		}
		name := hamiGpuInfo.CanonicalName()
		devices[name] = deviceInfo
		// }

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	// Debug:
	for name, _ := range devices {
		klog.Warningf("enumerateGpusDevicesForHAMiCore -- CanonicalName: %s", name)
	}

	return devices, nil
}

func (l deviceLib) enumerateGpusAndMigDevices(config *Config) (AllocatableDevices, error) {
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	devices := make(AllocatableDevices)
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		deviceInfo := &AllocatableDevice{
			Gpu: gpuInfo,
		}
		devices[gpuInfo.CanonicalName()] = deviceInfo

		migs, err := l.getMigDevices(gpuInfo)
		if err != nil {
			return fmt.Errorf("error getting MIG devices for GPU %d: %w", i, err)
		}

		for _, migDeviceInfo := range migs {
			deviceInfo := &AllocatableDevice{
				Mig: migDeviceInfo,
			}
			devices[migDeviceInfo.CanonicalName()] = deviceInfo
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	return devices, nil
}

func (l deviceLib) getGpuInfo(index int, device nvdev.Device) (*GpuInfo, error) {
	minor, ret := device.GetMinorNumber()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting minor number for device %d: %v", index, ret)
	}
	uuid, ret := device.GetUUID()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting UUID for device %d: %v", index, ret)
	}
	migEnabled, err := device.IsMigEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if MIG mode enabled for device %d: %w", index, err)
	}
	memory, ret := device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting memory info for device %d: %v", index, ret)
	}
	productName, ret := device.GetName()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting product name for device %d: %v", index, ret)
	}
	architecture, err := device.GetArchitectureAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting architecture for device %d: %w", index, err)
	}
	brand, err := device.GetBrandAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting brand for device %d: %w", index, err)
	}
	cudaComputeCapability, err := device.GetCudaComputeCapabilityAsString()
	if err != nil {
		return nil, fmt.Errorf("error getting CUDA compute capability for device %d: %w", index, err)
	}
	driverVersion, ret := l.nvmllib.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting driver version: %w", err)
	}
	cudaDriverVersion, ret := l.nvmllib.SystemGetCudaDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting CUDA driver version: %w", err)
	}

	pcieBusID, err := device.GetPCIBusID()
	if err != nil {
		return nil, fmt.Errorf("error getting PCIe bus ID for device %d: %w", index, err)
	}

	pcieRootAttr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(pcieBusID)
	if err != nil {
		return nil, fmt.Errorf("error getting PCIe root for device %d: %w", index, err)
	}

	var migProfiles []*MigProfileInfo
	for i := 0; i < nvml.GPU_INSTANCE_PROFILE_COUNT; i++ {
		giProfileInfo, ret := device.GetGpuInstanceProfileInfo(i)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstanceProfileInfo for profile %d on GPU %v", i, uuid)
		}

		giPossiblePlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("error retrieving GpuInstancePossiblePlacements for profile %d on GPU %v", i, uuid)
		}

		var migDevicePlacements []*MigDevicePlacement
		for _, p := range giPossiblePlacements {
			mdp := &MigDevicePlacement{
				GpuInstancePlacement: p,
			}
			migDevicePlacements = append(migDevicePlacements, mdp)
		}

		for j := 0; j < nvml.COMPUTE_INSTANCE_PROFILE_COUNT; j++ {
			for k := 0; k < nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_COUNT; k++ {
				migProfile, err := l.NewMigProfile(i, j, k, giProfileInfo.MemorySizeMB, memory.Total)
				if err != nil {
					return nil, fmt.Errorf("error building MIG profile from GpuInstanceProfileInfo for profile %d on GPU %v", i, uuid)
				}

				if migProfile.GetInfo().G != migProfile.GetInfo().C {
					continue
				}

				profileInfo := &MigProfileInfo{
					profile:    migProfile,
					placements: migDevicePlacements,
				}

				migProfiles = append(migProfiles, profileInfo)
			}
		}
	}

	gpuInfo := &GpuInfo{
		UUID:                  uuid,
		minor:                 minor,
		index:                 index,
		migEnabled:            migEnabled,
		memoryBytes:           memory.Total,
		productName:           productName,
		brand:                 brand,
		architecture:          architecture,
		cudaComputeCapability: cudaComputeCapability,
		driverVersion:         driverVersion,
		cudaDriverVersion:     fmt.Sprintf("%v.%v", cudaDriverVersion/1000, (cudaDriverVersion%1000)/10),
		pcieBusID:             pcieBusID,
		pcieRootAttr:          pcieRootAttr,
		migProfiles:           migProfiles,
	}

	return gpuInfo, nil
}

func (l deviceLib) getMigDevices(gpuInfo *GpuInfo) (map[string]*MigDeviceInfo, error) {
	if !gpuInfo.migEnabled {
		return nil, nil
	}

	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	device, ret := l.nvmllib.DeviceGetHandleByUUID(gpuInfo.UUID)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device handle: %v", ret)
	}

	migInfos := make(map[string]*MigDeviceInfo)
	err := walkMigDevices(device, func(i int, migDevice nvml.Device) error {
		giID, ret := migDevice.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance ID for MIG device: %v", ret)
		}
		gi, ret := device.GetGpuInstanceById(giID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance for '%v': %v", giID, ret)
		}
		giInfo, ret := gi.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting GPU instance info for '%v': %v", giID, ret)
		}
		ciID, ret := migDevice.GetComputeInstanceId()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance ID for MIG device: %v", ret)
		}
		ci, ret := gi.GetComputeInstanceById(ciID)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance for '%v': %v", ciID, ret)
		}
		ciInfo, ret := ci.GetInfo()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting Compute instance info for '%v': %v", ciID, ret)
		}
		uuid, ret := migDevice.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting UUID for MIG device: %v", ret)
		}

		var migProfile *MigProfileInfo
		var giProfileInfo *nvml.GpuInstanceProfileInfo
		var ciProfileInfo *nvml.ComputeInstanceProfileInfo
		for _, profile := range gpuInfo.migProfiles {
			profileInfo := profile.profile.GetInfo()
			gipInfo, ret := device.GetGpuInstanceProfileInfo(profileInfo.GIProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			if giInfo.ProfileId != gipInfo.Id {
				continue
			}
			cipInfo, ret := gi.GetComputeInstanceProfileInfo(profileInfo.CIProfileID, profileInfo.CIEngProfileID)
			if ret != nvml.SUCCESS {
				continue
			}
			if ciInfo.ProfileId != cipInfo.Id {
				continue
			}
			migProfile = profile
			giProfileInfo = &gipInfo
			ciProfileInfo = &cipInfo
		}
		if migProfile == nil {
			return fmt.Errorf("error getting profile info for MIG device: %v", uuid)
		}

		placement := MigDevicePlacement{
			GpuInstancePlacement: giInfo.Placement,
		}

		migInfos[uuid] = &MigDeviceInfo{
			UUID:          uuid,
			index:         i,
			profile:       migProfile.String(),
			parent:        gpuInfo,
			placement:     &placement,
			giProfileInfo: giProfileInfo,
			giInfo:        &giInfo,
			ciProfileInfo: ciProfileInfo,
			ciInfo:        &ciInfo,
			pcieBusID:     gpuInfo.pcieBusID,
			pcieRootAttr:  gpuInfo.pcieRootAttr,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error enumerating MIG devices: %w", err)
	}

	if len(migInfos) == 0 {
		return nil, nil
	}

	return migInfos, nil
}

func walkMigDevices(d nvml.Device, f func(i int, d nvml.Device) error) error {
	count, ret := nvml.Device(d).GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("error getting max MIG device count: %v", ret)
	}

	for i := 0; i < count; i++ {
		device, ret := d.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			continue
		}
		if ret == nvml.ERROR_INVALID_ARGUMENT {
			continue
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("error getting MIG device handle at index '%v': %v", i, ret)
		}
		err := f(i, device)
		if err != nil {
			return err
		}
	}
	return nil
}

func (l deviceLib) setTimeSlice(uuids []string, timeSlice int) error {
	for _, uuid := range uuids {
		cmd := exec.Command(
			l.nvidiaSMIPath,
			"compute-policy",
			"-i", uuid,
			"--set-timeslice", fmt.Sprintf("%d", timeSlice))

		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		output, err := cmd.CombinedOutput()
		if err != nil {
			klog.Errorf("\n%v", string(output))
			return fmt.Errorf("error running nvidia-smi: %w", err)
		}
	}
	return nil
}

func (l deviceLib) setComputeMode(uuids []string, mode string) error {
	for _, uuid := range uuids {
		cmd := exec.Command(
			l.nvidiaSMIPath,
			"-i", uuid,
			"-c", mode)

		// In order for nvidia-smi to run, we need update LD_PRELOAD to include the path to libnvidia-ml.so.1.
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		output, err := cmd.CombinedOutput()
		if err != nil {
			klog.Errorf("\n%v", string(output))
			return fmt.Errorf("error running nvidia-smi: %w", err)
		}
	}
	return nil
}
