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
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"

	appsv1 "k8s.io/api/apps/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "github.com/NVIDIA/k8s-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
)

const (
	MpsRoot                      = DriverPluginPath + "/mps"
	MpsControlDaemonTemplatePath = "/templates/mps-control-daemon.tmpl.yaml"
	MpsControlDaemonNameFmt      = "mps-control-daemon-%v" // Fill with ClaimUID
)

type HAMiCoreManager struct {
	hostHookPath string
	nvdevlib     *deviceLib
}

type TimeSlicingManager struct {
	nvdevlib *deviceLib
}

type MpsManager struct {
	config           *Config
	controlFilesRoot string
	hostDriverRoot   string
	templatePath     string

	nvdevlib *deviceLib
}

type MpsControlDaemon struct {
	id        string
	nodeName  string
	namespace string
	name      string
	rootDir   string
	pipeDir   string
	shmDir    string
	logDir    string
	devices   UUIDProvider
	manager   *MpsManager
}

type MpsControlDaemonTemplateData struct {
	NodeName                        string
	MpsControlDaemonNamespace       string
	MpsControlDaemonName            string
	CUDA_VISIBLE_DEVICES            string //nolint:stylecheck
	DefaultActiveThreadPercentage   string
	DefaultPinnedDeviceMemoryLimits map[string]string
	NvidiaDriverRoot                string
	MpsShmDirectory                 string
	MpsPipeDirectory                string
	MpsLogDirectory                 string
	MpsImageName                    string
}

func NewHAMiCoreManager(deviceLib *deviceLib) *HAMiCoreManager {
	return &HAMiCoreManager{
		nvdevlib:     deviceLib,
		hostHookPath: "/usr/local",
	}
}

func (m *HAMiCoreManager) getConsumableCapacityMap(claim *resourceapi.ResourceClaim) map[string]map[resourceapi.QualifiedName]resource.Quantity {
	resMap := map[string]map[resourceapi.QualifiedName]resource.Quantity{}
	for _, result := range claim.Status.Allocation.Devices.Results {
		devName := result.Device
		if _, exists := resMap[devName]; !exists {
			resMap[devName] = map[resourceapi.QualifiedName]resource.Quantity{}
		}
		maps.Copy(resMap[devName], result.ConsumedCapacity)
	}
	return resMap
}

func (m *HAMiCoreManager) GetCDIContainerEdits(claim *resourceapi.ResourceClaim, devs AllocatableDevices) *cdiapi.ContainerEdits {
	cacheFileHostDirectory := fmt.Sprintf("%s/vgpu/claims/%s", m.hostHookPath, claim.UID)
	// TODO: We should check the status of claim, becasue there may be two pod share the claim
	os.RemoveAll(cacheFileHostDirectory)
	os.MkdirAll(cacheFileHostDirectory, 0777)
	os.Chmod(cacheFileHostDirectory, 0777)

	hamiEnvs := []string{}
	// TOOD: Get SM Limit from Claim's Annotation
	hamiEnvs = append(hamiEnvs, fmt.Sprintf("CUDA_DEVICE_MEMORY_SHARED_CACHE=%s", fmt.Sprintf("%s/%v.cache", cacheFileHostDirectory, uuid.New().String())))

	devCapMap := m.getConsumableCapacityMap(claim)
	idx := 0
	for name, dev := range devs {
		// TODO: The idx here may not equals to the index in nvidia-smi, So we need to find a solution to solve it
		klog.Warningf("HAMiCoreManager GetCDIContainerEdits for dev: %s\n", name)
		capNameSMLimit := resourceapi.QualifiedName("cores")
		capNameMemoryLimit := resourceapi.QualifiedName("memory")
		SMLimitEnv := fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_%d=%s", idx, "60")
		memoryLimit := string(strconv.FormatUint(dev.HAMiGpu.memoryBytes/1024/1024, 10)) + "m"
		MemoryLimitEnv := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%d=%s", idx, memoryLimit)
		// TODO: Loop in a map getting from HAMiCoreManager
		if _, ok := devCapMap[name]; ok {
			if _, ok := devCapMap[name][capNameSMLimit]; ok {
				q := devCapMap[name][capNameSMLimit]
				val, succ := q.AsInt64()
				if succ {
					SMLimitEnv = fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_%d=%s", idx, strconv.FormatInt(val, 10))
				}
			}
			if _, ok := devCapMap[name][capNameMemoryLimit]; ok {
				q := devCapMap[name][capNameMemoryLimit]
				val, succ := q.AsInt64()
				if succ {
					MemoryLimitEnv = fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%d=%s", idx, strconv.FormatInt(val/1024/1024, 10)+"m")
				}
			}
		}
		hamiEnvs = append(hamiEnvs, SMLimitEnv, MemoryLimitEnv)
		idx++
	}

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: hamiEnvs,
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: cacheFileHostDirectory,
					HostPath:      cacheFileHostDirectory,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: m.hostHookPath + "/vgpu/libvgpu.so",
					HostPath:      m.hostHookPath + "/vgpu/libvgpu.so",
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				// TODO: Check CUDA_DISABLE_CONTROL env before mount ld.so.preload
				{
					ContainerPath: "/etc/ld.so.preload",
					HostPath:      m.hostHookPath + "/vgpu/ld.so.preload",
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: "/tmp/vgpulock",
					HostPath:      "/tmp/vgpulock",
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}
}

func (m *HAMiCoreManager) Cleanup(claimUID string, pl PreparedDeviceList) error {
	path := fmt.Sprintf("%s/vgpu/claims/%s", m.hostHookPath, claimUID)
	os.RemoveAll(path)
	return nil
}

func NewTimeSlicingManager(deviceLib *deviceLib) *TimeSlicingManager {
	return &TimeSlicingManager{
		nvdevlib: deviceLib,
	}
}

func (t *TimeSlicingManager) SetTimeSlice(devices UUIDProvider, config *configapi.TimeSlicingConfig) error {
	// Ensure all devices are full devices
	if !slices.Equal(devices.UUIDs(), devices.GpuUUIDs()) {
		return fmt.Errorf("can only set the time-slice interval on full GPUs")
	}

	// Set the compute mode of the GPU to DEFAULT.
	err := t.nvdevlib.setComputeMode(devices.UUIDs(), "DEFAULT")
	if err != nil {
		return fmt.Errorf("error setting compute mode: %w", err)
	}

	// Set the time slice based on the config provided.
	err = t.nvdevlib.setTimeSlice(devices.UUIDs(), config.Interval.Int())
	if err != nil {
		return fmt.Errorf("error setting time slice: %w", err)
	}

	return nil
}

func NewMpsManager(config *Config, deviceLib *deviceLib, controlFilesRoot, hostDriverRoot, templatePath string) *MpsManager {
	return &MpsManager{
		controlFilesRoot: controlFilesRoot,
		hostDriverRoot:   hostDriverRoot,
		templatePath:     templatePath,
		config:           config,
		nvdevlib:         deviceLib,
	}
}

func (m *MpsManager) NewMpsControlDaemon(claimUID string, devices UUIDProvider) *MpsControlDaemon {
	id := m.GetMpsControlDaemonID(claimUID, devices)

	return &MpsControlDaemon{
		id:        id,
		nodeName:  m.config.flags.nodeName,
		namespace: m.config.flags.namespace,
		name:      fmt.Sprintf(MpsControlDaemonNameFmt, id),
		rootDir:   fmt.Sprintf("%s/%s", m.controlFilesRoot, id),
		pipeDir:   fmt.Sprintf("%s/%s/%s", m.controlFilesRoot, id, "pipe"),
		shmDir:    fmt.Sprintf("%s/%s/%s", m.controlFilesRoot, id, "shm"),
		logDir:    fmt.Sprintf("%s/%s/%s", m.controlFilesRoot, id, "log"),
		devices:   devices,
		manager:   m,
	}
}

func (m *MpsManager) GetMpsControlDaemonID(claimUID string, devices UUIDProvider) string {
	combined := strings.Join(devices.UUIDs(), ",")
	hash := sha256.Sum256([]byte(combined))
	return fmt.Sprintf("%s-%s", claimUID, hex.EncodeToString(hash[:])[:5])
}

func (m *MpsManager) IsControlDaemonStarted(ctx context.Context, id string) (bool, error) {
	name := fmt.Sprintf(MpsControlDaemonNameFmt, id)
	_, err := m.config.clientsets.Core.AppsV1().Deployments(m.config.flags.namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get deployment: %w", err)
	}
	return true, nil
}

func (m *MpsManager) IsControlDaemonStopped(ctx context.Context, id string) (bool, error) {
	name := fmt.Sprintf(MpsControlDaemonNameFmt, id)
	_, err := m.config.clientsets.Core.AppsV1().Deployments(m.config.flags.namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get deployment: %w", err)
	}
	return false, nil
}

func (m *MpsControlDaemon) GetID() string {
	return m.id
}

func (m *MpsControlDaemon) Start(ctx context.Context, config *configapi.MpsConfig) error {
	isStarted, err := m.manager.IsControlDaemonStarted(ctx, m.id)
	if err != nil {
		return fmt.Errorf("error checking if control daemon already started: %w", err)
	}

	if isStarted {
		return nil
	}

	klog.Infof("Starting MPS control daemon for '%v', with settings: %+v", m.id, config)

	deviceUUIDs := m.devices.UUIDs()
	templateData := MpsControlDaemonTemplateData{
		NodeName:                        m.nodeName,
		MpsControlDaemonNamespace:       m.namespace,
		MpsControlDaemonName:            m.name,
		CUDA_VISIBLE_DEVICES:            strings.Join(deviceUUIDs, ","),
		DefaultActiveThreadPercentage:   "",
		DefaultPinnedDeviceMemoryLimits: nil,
		NvidiaDriverRoot:                m.manager.hostDriverRoot,
		MpsShmDirectory:                 m.shmDir,
		MpsPipeDirectory:                m.pipeDir,
		MpsLogDirectory:                 m.logDir,
		MpsImageName:                    m.manager.config.flags.imageName,
	}

	if config != nil && config.DefaultActiveThreadPercentage != nil {
		templateData.DefaultActiveThreadPercentage = fmt.Sprintf("%d", *config.DefaultActiveThreadPercentage)
	}

	if config != nil {
		limits, err := config.DefaultPerDevicePinnedMemoryLimit.Normalize(deviceUUIDs, config.DefaultPinnedDeviceMemoryLimit)
		if err != nil {
			return fmt.Errorf("error transforming DefaultPerDevicePinnedMemoryLimit into string: %w", err)
		}
		templateData.DefaultPinnedDeviceMemoryLimits = limits
	}

	tmpl, err := template.ParseFiles(m.manager.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template file: %w", err)
	}

	var deploymentYaml bytes.Buffer
	if err := tmpl.Execute(&deploymentYaml, templateData); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	var unstructuredObj unstructured.Unstructured
	err = yaml.Unmarshal(deploymentYaml.Bytes(), &unstructuredObj)
	if err != nil {
		return fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	var deployment appsv1.Deployment
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &deployment)
	if err != nil {
		return fmt.Errorf("failed to convert unstructured data to typed object: %w", err)
	}

	err = os.MkdirAll(m.shmDir, 0755)
	if err != nil {
		return fmt.Errorf("error creating directory %v: %w", m.shmDir, err)
	}

	err = os.MkdirAll(m.pipeDir, 0755)
	if err != nil {
		return fmt.Errorf("error creating directory %v: %w", m.pipeDir, err)
	}

	err = os.MkdirAll(m.logDir, 0755)
	if err != nil {
		return fmt.Errorf("error creating directory %v: %w", m.logDir, err)
	}

	mountExecutable, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("error finding 'mount' executable: %w", err)
	}

	mounter := mount.New(mountExecutable)
	sizeArg := fmt.Sprintf("size=%v", getDefaultShmSize())
	mountOptions := []string{"rw", "nosuid", "nodev", "noexec", "relatime", sizeArg}
	err = mounter.Mount("shm", m.shmDir, "tmpfs", mountOptions)
	if err != nil {
		return fmt.Errorf("error mounting %v as tmpfs: %w", m.shmDir, err)
	}

	err = m.manager.nvdevlib.setComputeMode(m.devices.GpuUUIDs(), "EXCLUSIVE_PROCESS")
	if err != nil {
		return fmt.Errorf("error setting compute mode: %w", err)
	}

	_, err = m.manager.config.clientsets.Core.AppsV1().Deployments(m.namespace).Create(ctx, &deployment, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	return nil
}

func (m *MpsControlDaemon) AssertReady(ctx context.Context) error {
	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   2,
		Jitter:   1,
		Steps:    4,
		Cap:      10 * time.Second,
	}

	return retry.OnError(
		backoff,
		func(error) bool {
			return true
		},
		func() error {
			deployment, err := m.manager.config.clientsets.Core.AppsV1().Deployments(m.namespace).Get(
				ctx,
				m.name,
				metav1.GetOptions{},
			)
			if err != nil {
				return fmt.Errorf("failed to get deployment: %w", err)
			}

			if deployment.Status.ReadyReplicas != 1 {
				return fmt.Errorf("waiting for MPS control daemon to come online")
			}

			selector := deployment.Spec.Selector.MatchLabels

			pods, err := m.manager.config.clientsets.Core.CoreV1().Pods(m.namespace).List(
				ctx,
				metav1.ListOptions{
					LabelSelector: labels.Set(selector).AsSelector().String(),
				},
			)
			if err != nil {
				return fmt.Errorf("error listing pods from deployment")
			}

			if len(pods.Items) != 1 {
				return fmt.Errorf("unexpected number of pods in deployment: %v", len(pods.Items))
			}

			if len(pods.Items[0].Status.ContainerStatuses) != 1 {
				return fmt.Errorf("unexpected number of container statuses in pod")
			}

			if !pods.Items[0].Status.ContainerStatuses[0].Ready {
				return fmt.Errorf("control daemon not yet ready")
			}

			return nil
		},
	)
}

func (m *MpsControlDaemon) GetCDIContainerEdits() *cdiapi.ContainerEdits {
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: []string{
				fmt.Sprintf("CUDA_MPS_PIPE_DIRECTORY=%s", "/tmp/nvidia-mps"),
			},
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: "/dev/shm",
					HostPath:      m.shmDir,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: "/tmp/nvidia-mps",
					HostPath:      m.pipeDir,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}
}

func (m *MpsControlDaemon) Stop(ctx context.Context) error {
	_, err := os.Stat(m.rootDir)
	if os.IsNotExist(err) {
		return nil
	}

	klog.Infof("Stopping MPS control daemon for '%v'", m.id)

	deletePolicy := metav1.DeletePropagationForeground
	deleteOptions := metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	err = m.manager.config.clientsets.Core.AppsV1().Deployments(m.namespace).Delete(ctx, m.name, deleteOptions)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete deployment: %w", err)
	}

	mountExecutable, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("error finding 'mount' executable: %w", err)
	}

	mounter := mount.New(mountExecutable)
	err = mount.CleanupMountPoint(m.shmDir, mounter, true)
	if err != nil {
		return fmt.Errorf("error unmounting %v: %w", m.shmDir, err)
	}

	err = os.RemoveAll(m.rootDir)
	if err != nil {
		return fmt.Errorf("error removing directory %v: %w", m.rootDir, err)
	}

	return nil
}

// getDefaultShmSize returns the default size for the tmpfs to be created.
// This reads /proc/meminfo to get the total memory to calculate this. If this
// fails a fallback size of 65536k is used.
func getDefaultShmSize() string {
	const fallbackSize = "65536k"

	meminfo, err := os.Open("/proc/meminfo")
	if err != nil {
		klog.ErrorS(err, "failed to open /proc/meminfo")
		return fallbackSize
	}
	defer func() {
		_ = meminfo.Close()
	}()

	scanner := bufio.NewScanner(meminfo)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}

		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:")), " ", 2)
		memTotal, err := strconv.Atoi(parts[0])
		if err != nil {
			klog.ErrorS(err, "could not convert MemTotal to an integer")
			return fallbackSize
		}

		var unit string
		if len(parts) == 2 {
			unit = string(parts[1][0])
		}

		return fmt.Sprintf("%d%s", memTotal/2, unit)
	}
	return fallbackSize
}
