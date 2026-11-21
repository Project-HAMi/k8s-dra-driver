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
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	nvmlmock "github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/stretchr/testify/require"
)

func TestGetGpuInfoIndex(t *testing.T) {
	tests := []struct {
		name      string
		minor     int
		nvmlIndex int
		indexRet  nvml.Return
		wantErr   string
	}{
		{
			name:      "distinct minor and nvml index",
			minor:     3,
			nvmlIndex: 1,
			indexRet:  nvml.SUCCESS,
		},
		{
			name:     "get index error",
			minor:    3,
			indexRet: nvml.ERROR_UNKNOWN,
			wantErr:  "error getting index for device 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockNvml := &nvmlmock.Interface{
				SystemGetDriverVersionFunc: func() (string, nvml.Return) {
					return "550.54.15", nvml.SUCCESS
				},
				SystemGetCudaDriverVersionFunc: func() (int, nvml.Return) {
					return 12040, nvml.SUCCESS
				},
			}

			var pciInfo nvml.PciInfo
			copy(pciInfo.BusId[:], "00000000:00:1E.0")

			mockDev := &nvmlmock.Device{
				GetMinorNumberFunc: func() (int, nvml.Return) {
					return tt.minor, nvml.SUCCESS
				},
				GetIndexFunc: func() (int, nvml.Return) {
					return tt.nvmlIndex, tt.indexRet
				},
				GetUUIDFunc: func() (string, nvml.Return) {
					return "GPU-test-uuid", nvml.SUCCESS
				},
				GetMigModeFunc: func() (int, int, nvml.Return) {
					return nvml.DEVICE_MIG_DISABLE, nvml.DEVICE_MIG_DISABLE, nvml.ERROR_NOT_SUPPORTED
				},
				GetMemoryInfoFunc: func() (nvml.Memory, nvml.Return) {
					return nvml.Memory{Total: testGPUMemory}, nvml.SUCCESS
				},
				GetNameFunc: func() (string, nvml.Return) {
					return "Tesla T4", nvml.SUCCESS
				},
				GetArchitectureFunc: func() (nvml.DeviceArchitecture, nvml.Return) {
					return nvml.DEVICE_ARCH_TURING, nvml.SUCCESS
				},
				GetBrandFunc: func() (nvml.BrandType, nvml.Return) {
					return nvml.BRAND_TESLA, nvml.SUCCESS
				},
				GetCudaComputeCapabilityFunc: func() (int, int, nvml.Return) {
					return 7, 5, nvml.SUCCESS
				},
				GetPciInfoFunc: func() (nvml.PciInfo, nvml.Return) {
					return pciInfo, nvml.SUCCESS
				},
				GetGpuInstanceProfileInfoFunc: func(int) (nvml.GpuInstanceProfileInfo, nvml.Return) {
					return nvml.GpuInstanceProfileInfo{}, nvml.ERROR_NOT_SUPPORTED
				},
			}

			nvlib := nvdev.New(mockNvml, nvdev.WithVerifySymbols(false))
			dev, err := nvlib.NewDevice(mockDev)
			require.NoError(t, err)

			lib := deviceLib{
				Interface: nvlib,
				nvmllib:   mockNvml,
			}

			info, err := lib.getGpuInfo(0, dev)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.minor, info.minor)
			require.Equal(t, tt.nvmlIndex, info.index)
		})
	}
}
