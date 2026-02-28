package host

type Metrics struct {
	Timestamp int64 `json:"ts"` // Unix Timestamp in UTC

	CPUCount       uint32  `json:"cpu_count"`    // Total CPU cores
	CPUUsedPercent float32 `json:"cpu_used_pct"` // Percent rounded to 2 decimal places

	// Deprecated
	// TODO: Remove when they are no longer used in orchestrator (https://linear.app/e2b/issue/E2B-2998/remove-envd-deprecated-metrics-when-not-used)
	MemTotalMiB uint64 `json:"mem_total_mib"` // Total virtual memory in MiB

	// Deprecated
	// TODO: Remove when no longer used in orchestrator
	MemUsedMiB uint64 `json:"mem_used_mib"` // Used virtual memory in MiB

	MemTotal uint64 `json:"mem_total"` // Total virtual memory in bytes
	MemUsed  uint64 `json:"mem_used"`  // Used virtual memory in bytes

	DiskUsed  uint64 `json:"disk_used"`  // Used disk space in bytes
	DiskTotal uint64 `json:"disk_total"` // Total disk space in bytes
}

type diskSpace struct {
	Total     uint64
	Available uint64
}
