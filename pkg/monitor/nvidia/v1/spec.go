/*
Copyright 2024 The HAMi Authors.

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

// Package v1 overlays the current libvgpu shared-region layout (majorVersion == 1).
// Matches HAMi-core MAJOR_VERSION 1 / MINOR_VERSION 2 (includes lastKernelTime + padding).
//
// Do not change struct field order/sizes without matching HAMi-core
// shared_region_t; Spec methods reinterpret mmap'd bytes via unsafe.
package v1

import "unsafe"

const maxDevices = 16

type deviceMemory struct {
	contextSize uint64
	moduleSize  uint64
	bufferSize  uint64
	offset      uint64
	total       uint64
	unused      [3]uint64
}

type deviceUtilization struct {
	decUtil uint64
	encUtil uint64
	smUtil  uint64
	unused  [3]uint64
}

type shrregProcSlotT struct {
	pid         int32
	hostpid     int32
	used        [16]deviceMemory
	monitorused [16]uint64
	deviceUtil  [16]deviceUtilization
	status      int32
	unused      [3]uint64
}

type uuid struct {
	uuid [96]byte
}

type semT struct {
	sem [32]byte
}

type sharedRegionT struct {
	initializedFlag int32
	majorVersion    int32
	minorVersion    int32
	smInitFlag      int32
	ownerPid        uint32
	sem             semT
	num             uint64
	uuids           [16]uuid

	limit   [16]uint64
	smLimit [16]uint64
	procs   [1024]shrregProcSlotT

	procnum           int32
	utilizationSwitch int32
	recentKernel      int32
	priority          int32
	lastKernelTime    int64
	unused            [4]uint64
}

// Spec overlays a mmap'd v1 shared_region_t.
type Spec struct {
	sr *sharedRegionT
}

// DeviceMax returns the fixed device-slot capacity of the shared region.
func (s Spec) DeviceMax() int {
	return maxDevices
}

// DeviceNum returns the number of devices recorded in the shared region, clamped to DeviceMax.
func (s Spec) DeviceNum() int {
	n := int(s.sr.num)
	if n > maxDevices {
		return maxDevices
	}
	if n < 0 {
		return 0
	}
	return n
}

func (s Spec) activeProcs() []shrregProcSlotT {
	n := int(s.sr.procnum)
	if n < 0 {
		return nil
	}
	if n > len(s.sr.procs) {
		n = len(s.sr.procs)
	}
	return s.sr.procs[:n]
}

// DeviceMemoryContextSize returns total context memory usage for device idx across active procs.
func (s Spec) DeviceMemoryContextSize(idx int) uint64 {
	v := uint64(0)
	for _, p := range s.activeProcs() {
		v += p.used[idx].contextSize
	}
	return v
}

// DeviceMemoryModuleSize returns total module memory usage for device idx across active procs.
func (s Spec) DeviceMemoryModuleSize(idx int) uint64 {
	v := uint64(0)
	for _, p := range s.activeProcs() {
		v += p.used[idx].moduleSize
	}
	return v
}

// DeviceMemoryBufferSize returns total buffer memory usage for device idx across active procs.
func (s Spec) DeviceMemoryBufferSize(idx int) uint64 {
	v := uint64(0)
	for _, p := range s.activeProcs() {
		v += p.used[idx].bufferSize
	}
	return v
}

// DeviceMemoryOffset returns total memory offset for device idx across active procs.
func (s Spec) DeviceMemoryOffset(idx int) uint64 {
	v := uint64(0)
	for _, p := range s.activeProcs() {
		v += p.used[idx].offset
	}
	return v
}

// DeviceMemoryTotal returns total memory usage for device idx across active procs.
func (s Spec) DeviceMemoryTotal(idx int) uint64 {
	v := uint64(0)
	for _, p := range s.activeProcs() {
		v += p.used[idx].total
	}
	return v
}

// DeviceSmUtil returns total SM utilization for device idx across active procs.
func (s Spec) DeviceSmUtil(idx int) uint64 {
	v := uint64(0)
	for _, p := range s.activeProcs() {
		v += p.deviceUtil[idx].smUtil
	}
	return v
}

// SetDeviceSmLimit sets the SM limit for every active device slot.
func (s Spec) SetDeviceSmLimit(l uint64) {
	n := uint64(s.DeviceNum())
	for idx := uint64(0); idx < n; idx++ {
		s.sr.smLimit[idx] = l
	}
}

// IsValidUUID reports whether device idx has a non-empty UUID.
func (s Spec) IsValidUUID(idx int) bool {
	return s.sr.uuids[idx].uuid[0] != 0
}

// DeviceUUID returns the raw UUID bytes for device idx as a string.
func (s Spec) DeviceUUID(idx int) string {
	return string(s.sr.uuids[idx].uuid[:])
}

// DeviceMemoryLimit returns the memory limit for device idx.
func (s Spec) DeviceMemoryLimit(idx int) uint64 {
	return s.sr.limit[idx]
}

// SetDeviceMemoryLimit sets the memory limit for every active device slot.
func (s Spec) SetDeviceMemoryLimit(l uint64) {
	n := uint64(s.DeviceNum())
	for idx := uint64(0); idx < n; idx++ {
		s.sr.limit[idx] = l
	}
}

// LastKernelTime returns the last kernel timestamp from the shared region.
func (s Spec) LastKernelTime() int64 {
	return s.sr.lastKernelTime
}

// SpecSize is the byte size of the shared region this package overlays.
// Callers must not CastSpec a mapping smaller than this: Spec methods read the
// whole region, so a short mapping would fault.
func SpecSize() int {
	return int(unsafe.Sizeof(sharedRegionT{}))
}

// CastSpec reinterprets mmap'd cache bytes as the v1 shared region.
// data must remain alive for the lifetime of the returned Spec (same backing mmap)
// and must be at least SpecSize() bytes.
func CastSpec(data []byte) Spec {
	return Spec{
		sr: (*sharedRegionT)(unsafe.Pointer(&data[0])),
	}
}

// GetPriority returns the shared-region task priority.
func (s Spec) GetPriority() int {
	return int(s.sr.priority)
}

// GetRecentKernel returns the recent-kernel flag from the shared region.
func (s Spec) GetRecentKernel() int32 {
	return s.sr.recentKernel
}

// SetRecentKernel sets the recent-kernel flag in the shared region.
func (s Spec) SetRecentKernel(v int32) {
	s.sr.recentKernel = v
}

// GetUtilizationSwitch returns the utilization switch from the shared region.
func (s Spec) GetUtilizationSwitch() int32 {
	return s.sr.utilizationSwitch
}

// SetUtilizationSwitch sets the utilization switch in the shared region.
func (s Spec) SetUtilizationSwitch(v int32) {
	s.sr.utilizationSwitch = v
}
