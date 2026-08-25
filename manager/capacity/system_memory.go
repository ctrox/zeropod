package capacity

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type SystemMemoryTracker struct {
	Tracker
}

// NewSystemMemoryTracker creates a [SystemMemoryTracker].
func NewSystemMemoryTracker(reg prometheus.Registerer, name string, threshold float64) Tracker {
	return &SystemMemoryTracker{Tracker: NewNodeTracker(reg, name, threshold)}
}

// Capacity gets the memory capacity from the total node memory.
func (m *SystemMemoryTracker) Capacity(name corev1.ResourceName) resource.Quantity {
	if name == corev1.ResourceMemory {
		return getTotalNodeMemory()
	}
	return m.Tracker.Capacity(name)
}

// Requested gets memory requests from the current available system memory.
func (m *SystemMemoryTracker) Requested(name corev1.ResourceName) resource.Quantity {
	if name == corev1.ResourceMemory {
		return getCurrentNodeMemoryUsage()
	}
	return m.Tracker.Requested(name)
}

// SetCapacity sets the memory capacity to the total node memory.
func (m *SystemMemoryTracker) SetCapacity(name corev1.ResourceName, q resource.Quantity) {
	if name == corev1.ResourceMemory {
		q = getTotalNodeMemory()
	}
	m.Tracker.SetCapacity(name, q)
}

// SetRequested sets memory requests to the current available system memory.
func (m *SystemMemoryTracker) SetRequested(name corev1.ResourceName, q resource.Quantity) {
	if name == corev1.ResourceMemory {
		q = getCurrentNodeMemoryUsage()
	}
	m.Tracker.SetRequested(name, q)
}

func (m *SystemMemoryTracker) UseCheckpointMemory() bool {
	return true
}

func getTotalNodeMemory() resource.Quantity {
	memStats, err := sysMemoryStats()
	if err != nil {
		return resource.Quantity{}
	}
	return *resource.NewQuantity(int64(memStats.totalBytes), resource.BinarySI)
}

func getCurrentNodeMemoryUsage() resource.Quantity {
	memStats, err := sysMemoryStats()
	if err != nil {
		return resource.Quantity{}
	}
	return *resource.NewQuantity(int64(memStats.usedBytes), resource.BinarySI)
}

type sysMemory struct {
	totalBytes     uint64
	availableBytes uint64
	usedBytes      uint64
}

func sysMemoryStats() (*sysMemory, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	//nolint:errcheck
	defer file.Close()

	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		key := strings.TrimSuffix(fields[0], ":")
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}

		switch key {
		case "MemTotal":
			memTotal = val * 1024
		case "MemAvailable":
			memAvailable = val * 1024
		}

		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return &sysMemory{
		totalBytes:     memTotal,
		availableBytes: memAvailable,
		usedBytes:      memTotal - memAvailable,
	}, nil
}
