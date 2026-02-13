package host

import (
	"math"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/unix"
)

const (
	E2BRunDir = "/run/e2b" // store sandbox metadata files here
)

func getMetrics() (*Metrics, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	memUsedMiB := v.Used / 1024 / 1024
	memTotalMiB := v.Total / 1024 / 1024

	cpuTotal, err := cpu.Counts(true)
	if err != nil {
		return nil, err
	}

	cpuUsedPcts, err := cpu.Percent(0, false)
	if err != nil {
		return nil, err
	}

	cpuUsedPct := cpuUsedPcts[0]
	cpuUsedPctRounded := float32(cpuUsedPct)
	if cpuUsedPct > 0 {
		cpuUsedPctRounded = float32(math.Round(cpuUsedPct*100) / 100)
	}

	diskMetrics, err := diskStats("/")
	if err != nil {
		return nil, err
	}

	return &Metrics{
		Timestamp:      time.Now().UTC().Unix(),
		CPUCount:       uint32(cpuTotal),
		CPUUsedPercent: cpuUsedPctRounded,
		MemUsedMiB:     memUsedMiB,
		MemTotalMiB:    memTotalMiB,
		MemTotal:       v.Total,
		MemUsed:        v.Used,
		DiskUsed:       diskMetrics.Total - diskMetrics.Available,
		DiskTotal:      diskMetrics.Total,
	}, nil
}

func diskStats(path string) (diskSpace, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return diskSpace{}, err
	}

	block := uint64(st.Bsize)

	// all data blocks
	total := st.Blocks * block
	// blocks available
	available := st.Bavail * block

	return diskSpace{Total: total, Available: available}, nil
}

type RealMetricsProvider struct{}

func (r *RealMetricsProvider) GetMetrics() (*Metrics, error) {
	return getMetrics()
}

var metricsProvider MetricsProvider = &RealMetricsProvider{}

func SetMetricsProvider(p MetricsProvider) {
	metricsProvider = p
}

func GetMetricsProvider() MetricsProvider {
	return metricsProvider
}
