// Package health collects system telemetry (CPU load, RAM usage, Disk space, Uptime)
// on Windows machines using native Win32 APIs with graceful cross-platform fallbacks.
package health

import (
	"net"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"terminalagent/internal/protocol"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount64    = kernel32.NewProc("GetTickCount64")
	procGetSystemTimes     = kernel32.NewProc("GetSystemTimes")

	cpuMu        sync.Mutex
	lastIdle     uint64
	lastKernel   uint64
	lastUser     uint64
	lastSampled  time.Time
	lastCPUUsage float64
)

type memorystatusex struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

type filetime struct {
	dwLowDateTime  uint32
	dwHighDateTime uint32
}

func (ft filetime) toUint64() uint64 {
	return (uint64(ft.dwHighDateTime) << 32) | uint64(ft.dwLowDateTime)
}

// GetPrimaryMACAddress returns the physical MAC address of the active network adapter.
func GetPrimaryMACAddress() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	// Prefer non-loopback up interfaces with 6-byte hardware address
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 && iface.Flags&net.FlagUp != 0 && len(iface.HardwareAddr) == 6 {
			return iface.HardwareAddr.String()
		}
	}
	// Fallback to any non-empty hardware address
	for _, iface := range interfaces {
		if len(iface.HardwareAddr) == 6 && iface.Flags&net.FlagLoopback == 0 {
			return iface.HardwareAddr.String()
		}
	}
	return ""
}

// Collect returns a snapshot of current system health metrics.
func Collect() protocol.HealthMetrics {
	var m protocol.HealthMetrics

	if runtime.GOOS == "windows" {
		m.CPUPercent = getWindowsCPU()
		m.RAMPercent, m.RAMUsedBytes, m.RAMTotalBytes = getWindowsMemory()
		m.DiskUsedBytes, m.DiskTotalBytes = getWindowsDisk()
		m.UptimeSec = getWindowsUptime()
	} else {
		// Fallback for non-Windows platforms
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		m.RAMUsedBytes = mem.Alloc
		m.RAMTotalBytes = mem.Sys
		if m.RAMTotalBytes > 0 {
			m.RAMPercent = float64(m.RAMUsedBytes) / float64(m.RAMTotalBytes) * 100
		}
		m.CPUPercent = 5.0
		m.UptimeSec = int64(time.Since(lastSampled).Seconds())
	}

	m.MACAddress = GetPrimaryMACAddress()
	m.ProcessCount = runtime.NumGoroutine()
	return m
}

func getWindowsMemory() (percent float64, usedBytes, totalBytes uint64) {
	if procGlobalMemoryStatus.Find() != nil {
		return 0, 0, 0
	}
	var mem memorystatusex
	mem.dwLength = uint32(unsafe.Sizeof(mem))
	ret, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&mem)))
	if ret == 0 {
		return 0, 0, 0
	}
	totalBytes = mem.ullTotalPhys
	usedBytes = mem.ullTotalPhys - mem.ullAvailPhys
	if totalBytes > 0 {
		percent = float64(usedBytes) / float64(totalBytes) * 100
	}
	return percent, usedBytes, totalBytes
}

func getWindowsDisk() (usedBytes, totalBytes uint64) {
	if procGetDiskFreeSpaceEx.Find() != nil {
		return 0, 0
	}
	pathPtr, err := syscall.UTF16PtrFromString("C:\\")
	if err != nil {
		return 0, 0
	}
	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	ret, _, _ := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalNumberOfBytes)),
		uintptr(unsafe.Pointer(&totalNumberOfFreeBytes)),
	)
	if ret == 0 {
		return 0, 0
	}
	usedBytes = totalNumberOfBytes - totalNumberOfFreeBytes
	return usedBytes, totalNumberOfBytes
}

func getWindowsUptime() int64 {
	if procGetTickCount64.Find() != nil {
		return 0
	}
	ticks, _, _ := procGetTickCount64.Call()
	return int64(ticks / 1000)
}

func getWindowsCPU() float64 {
	if procGetSystemTimes.Find() != nil {
		return 0
	}

	cpuMu.Lock()
	defer cpuMu.Unlock()

	var idleTime, kernelTime, userTime filetime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleTime)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	)
	if ret == 0 {
		return lastCPUUsage
	}

	idle := idleTime.toUint64()
	kernel := kernelTime.toUint64()
	user := userTime.toUint64()

	if lastSampled.IsZero() {
		lastIdle = idle
		lastKernel = kernel
		lastUser = user
		lastSampled = time.Now()
		return 0
	}

	idleDiff := idle - lastIdle
	kernelDiff := kernel - lastKernel
	userDiff := user - lastUser

	lastIdle = idle
	lastKernel = kernel
	lastUser = user
	lastSampled = time.Now()

	// In Windows GetSystemTimes, Kernel time includes Idle time
	totalDiff := kernelDiff + userDiff
	if totalDiff == 0 {
		return lastCPUUsage
	}

	busyDiff := totalDiff - idleDiff
	cpuPercent := (float64(busyDiff) / float64(totalDiff)) * 100.0
	if cpuPercent < 0 {
		cpuPercent = 0
	} else if cpuPercent > 100 {
		cpuPercent = 100
	}
	lastCPUUsage = cpuPercent
	return cpuPercent
}
