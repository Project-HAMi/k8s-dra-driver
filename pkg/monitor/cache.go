/*
 * Copyright 2025 The HAMi Authors.
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

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"k8s.io/klog/v2"
)

// ClaimUsage wraps a single mmaped cache file for a ResourceClaim.
type ClaimUsage struct {
	ClaimUID string
	CacheFile string
	data     []byte
	Info     UsageInfo
}

// ClaimLister scans the host vGPU claims directory, discovers .cache
// files created by libvgpu.so, and mmap(2)s them for live access.
type ClaimLister struct {
	hookPath string
	claims   map[string]*ClaimUsage // key: <claimUID>/<cacheFileName>
	mutex    sync.Mutex
}

// NewClaimLister creates a new ClaimLister rooted at hookPath.
// The effective scan path is <hookPath>/vgpu/claims/.
func NewClaimLister(hookPath string) *ClaimLister {
	return &ClaimLister{
		hookPath: hookPath,
		claims:   make(map[string]*ClaimUsage),
	}
}

// ListClaims returns a shallow copy of the currently known claims.
func (l *ClaimLister) ListClaims() map[string]*ClaimUsage {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	out := make(map[string]*ClaimUsage, len(l.claims))
	for k, v := range l.claims {
		out[k] = v
	}
	return out
}

// Update rescans the claims directory, mmaping new cache files and
// munmapping ones that have disappeared.
func (l *ClaimLister) Update() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	claimsPath := filepath.Join(l.hookPath, "vgpu", "claims")
	entries, err := os.ReadDir(claimsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No claims directory yet; clear everything.
			l.cleanupAll()
			return nil
		}
		return err
	}

	seen := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		claimUID := entry.Name()
		claimDir := filepath.Join(claimsPath, claimUID)
		cacheFile, err := findCacheFile(claimDir)
		if err != nil {
			klog.V(4).InfoS("No cache file in claim dir", "dir", claimDir, "err", err)
			continue
		}
		if cacheFile == "" {
			continue
		}
		key := claimUID + "/" + filepath.Base(cacheFile)
		seen[key] = struct{}{}

		if _, ok := l.claims[key]; ok {
			continue
		}

		usage, err := loadCache(claimUID, cacheFile)
		if err != nil {
			klog.ErrorS(err, "Failed to load cache", "file", cacheFile)
			continue
		}
		l.claims[key] = usage
		klog.V(3).InfoS("Loaded claim cache", "claim", claimUID, "file", cacheFile)
	}

	// Remove disappeared claims.
	for key, usage := range l.claims {
		if _, ok := seen[key]; !ok {
			_ = syscall.Munmap(usage.data)
			delete(l.claims, key)
			klog.V(3).InfoS("Removed claim cache", "key", key)
		}
	}

	return nil
}

func (l *ClaimLister) cleanupAll() {
	for key, usage := range l.claims {
		_ = syscall.Munmap(usage.data)
		delete(l.claims, key)
		klog.V(3).InfoS("Cleaned up claim cache", "key", key)
	}
}

func findCacheFile(dir string) (string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if strings.HasSuffix(f.Name(), ".cache") {
			return filepath.Join(dir, f.Name()), nil
		}
	}
	return "", nil
}

func loadCache(claimUID, cacheFile string) (*ClaimUsage, error) {
	info, err := os.Stat(cacheFile)
	if err != nil {
		return nil, fmt.Errorf("stat cache file: %w", err)
	}
	minSize := int64(unsafe.Sizeof(headerT{}))
	if info.Size() < minSize {
		return nil, fmt.Errorf("cache file size %d too small (need >= %d)", info.Size(), minSize)
	}

	f, err := os.OpenFile(cacheFile, os.O_RDWR, 0666)
	if err != nil {
		return nil, fmt.Errorf("open cache file: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_WRITE|syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap cache file: %w", err)
	}

	head := (*headerT)(unsafe.Pointer(&data[0]))
	if head.initializedFlag != SharedRegionMagicFlag {
		_ = syscall.Munmap(data)
		return nil, fmt.Errorf("cache file magic flag not matched: got %d, want %d", head.initializedFlag, SharedRegionMagicFlag)
	}

	var usageInfo UsageInfo
	switch {
	case info.Size() == 1197897:
		klog.V(4).InfoS("Detected v0 cache file", "file", cacheFile)
		usageInfo = castV0Spec(data)
	case head.majorVersion == 1:
		klog.V(4).InfoS("Detected v1 cache file", "file", cacheFile, "minorVersion", head.minorVersion)
		usageInfo = castV1Spec(data)
	default:
		_ = syscall.Munmap(data)
		return nil, fmt.Errorf("unknown cache version %d.%d size %d", head.majorVersion, head.minorVersion, info.Size())
	}

	return &ClaimUsage{
		ClaimUID:  claimUID,
		CacheFile: cacheFile,
		data:      data,
		Info:      usageInfo,
	}, nil
}
