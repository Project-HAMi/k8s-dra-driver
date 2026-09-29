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
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	"github.com/NVIDIA/k8s-dra-driver-gpu/pkg/flock"
)

const (
	testClaimUID       = "claim-uid"
	testBootID         = "boot-current"
	testPreviousBootID = "boot-previous"
	testCDIDeviceID    = "k8s.hami-core-gpu.project-hami.io/claim=claim-uid-gpu-0"
)

func newTestDeviceState(t *testing.T) *DeviceState {
	dir := t.TempDir()
	cm, err := checkpointmanager.NewCheckpointManager(dir)
	require.NoError(t, err)
	return &DeviceState{
		checkpointManager: cm,
		cplock:            flock.NewFlock(filepath.Join(dir, "cp.lock")),
	}
}

func completedHAMiClaim() PreparedClaim {
	return PreparedClaim{
		CheckpointState: ClaimCheckpointStatePrepareCompleted,
		PreparedDevices: PreparedDevices{{
			Devices: PreparedDeviceList{{
				HAMiGpu: &PreparedHAMiGpu{
					Device: &kubeletplugin.Device{
						Requests:     []string{"gpu"},
						PoolName:     "node",
						DeviceName:   "gpu-0",
						CDIDeviceIDs: []string{testCDIDeviceID},
					},
				},
			}},
		}},
	}
}

func TestInitCheckpoint(t *testing.T) {
	tests := []struct {
		name       string
		existing   *CheckpointV2
		wantClaims bool
	}{
		{
			name: "no checkpoint",
		},
		{
			name:       "same boot keeps prepared claims",
			existing:   &CheckpointV2{NodeBootID: testBootID},
			wantClaims: true,
		},
		{
			name:       "legacy checkpoint without boot ID keeps prepared claims",
			existing:   &CheckpointV2{},
			wantClaims: true,
		},
		{
			name:     "reboot discards prepared claims",
			existing: &CheckpointV2{NodeBootID: testPreviousBootID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := newTestDeviceState(t)
			if tt.existing != nil {
				tt.existing.PreparedClaims = PreparedClaimsByUID{testClaimUID: completedHAMiClaim()}
				require.NoError(t, s.createCheckpoint(ctx, &Checkpoint{V2: tt.existing}))
			}

			require.NoError(t, s.initCheckpoint(ctx, testBootID))

			cp, err := s.getCheckpoint(ctx)
			require.NoError(t, err)
			require.Equal(t, testBootID, cp.GetNodeBootID())
			_, exists := cp.V2.PreparedClaims[testClaimUID]
			require.Equal(t, tt.wantClaims, exists)
		})
	}
}

func TestPrepareReturnsCompletedHAMiClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestDeviceState(t)
	require.NoError(t, s.createCheckpoint(ctx, &Checkpoint{V2: &CheckpointV2{
		NodeBootID:     testBootID,
		PreparedClaims: PreparedClaimsByUID{testClaimUID: completedHAMiClaim()},
	}}))

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "claim", UID: testClaimUID},
	}
	devices, err := s.Prepare(ctx, claim)
	require.NoError(t, err)
	require.Len(t, devices, 1)
	require.Equal(t, []string{testCDIDeviceID}, devices[0].CDIDeviceIDs)
}
