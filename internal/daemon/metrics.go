package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"strings"
	"sync"
	"time"
)

type MetricsPayload struct {
	SentAt             time.Time   `json:"sent_at"`
	HostID             string      `json:"host_id,omitempty"`
	UptimeSeconds      int64       `json:"uptime_seconds"`
	ProcessMemoryBytes int64       `json:"process_memory_bytes"`
	CPUPercent         float64     `json:"cpu_percent"`
	CPUCount           int         `json:"cpu_count"`
	Load1              float64     `json:"load1"`
	Load5              float64     `json:"load5"`
	Load15             float64     `json:"load15"`
	MemoryUsedPercent  float64     `json:"memory_used_percent"`
	MemoryUsedBytes    uint64      `json:"memory_used_bytes"`
	MemoryTotalBytes   uint64      `json:"memory_total_bytes"`
	SwapTotalKb        int64       `json:"swap_total_kb"`
	SwapFreeKb         int64       `json:"swap_free_kb"`
	DiskTotalKb        int64       `json:"disk_total_kb"`
	DiskAvailableKb    int64       `json:"disk_available_kb"`
	Disks              []DiskUsage `json:"disks,omitempty"`
	NetRxBytes         uint64      `json:"net_rx_bytes"`
	NetTxBytes         uint64      `json:"net_tx_bytes"`
	WebhookExecutions  uint64      `json:"webhook_executions"`
	WebhookFailures    uint64      `json:"webhook_failures"`
}

// DiskUsage is one mounted filesystem's capacity snapshot. AvailableKb is the
// free space gopsutil reports (f_bfree, reserved blocks included), so used =
// total - available stays consistent with the aggregate root fields.
type DiskUsage struct {
	Mount       string `json:"mount"`
	Filesystem  string `json:"filesystem,omitempty"`
	TotalKb     int64  `json:"total_kb"`
	AvailableKb int64  `json:"available_kb"`
}

const (
	defaultMetricsRetentionDays = 7
	maxMetricsRetentionDays     = 30
	metricsDayFormat            = "2006-01-02"
)

type MetricsHistoryQuery struct {
	From   *time.Time
	To     *time.Time
	Before *time.Time
	Limit  int
}

type MetricsCollector struct {
	executor   *WebhookExecutor
	hostID     string
	mu         sync.RWMutex
	history    []MetricsPayload
	storageDir string
	retention  time.Duration
}

func NewMetricsCollector(cfg config.DaemonConfig, executor *WebhookExecutor) (*MetricsCollector, error) {
	retentionDays := cfg.MetricsRetentionDays
	if retentionDays <= 0 {
		retentionDays = defaultMetricsRetentionDays
	}
	if retentionDays > maxMetricsRetentionDays {
		retentionDays = maxMetricsRetentionDays
	}
	storageDir := strings.TrimSpace(cfg.MetricsHistoryPath)
	if strings.HasSuffix(storageDir, ".jsonl") {
		storageDir = strings.TrimSuffix(storageDir, ".jsonl")
	}
	collector := &MetricsCollector{
		executor:   executor,
		hostID:     strings.TrimSpace(cfg.HostID),
		history:    make([]MetricsPayload, 0),
		storageDir: storageDir,
		retention:  time.Duration(retentionDays) * 24 * time.Hour,
	}
	if storageDir == "" {
		return collector, nil
	}
	if err := collector.loadHistory(); err != nil {
		return nil, err
	}
	return collector, nil
}

// Record collects one snapshot and persists it in the current day's file.
func (m *MetricsCollector) Record() MetricsPayload {
	payload := m.Collect()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, payload)
	pruned := m.pruneLocked(payload.SentAt)
	if err := m.appendLocked(payload); err != nil {
		return payload
	}
	if pruned {
		_ = m.compactStorageLocked()
	}
	return payload
}

// History returns matching samples in chronological order.
func (m *MetricsCollector) History(query MetricsHistoryQuery) []MetricsPayload {
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	matching := make([]MetricsPayload, 0, limit)
	for _, sample := range m.history {
		if query.From != nil && sample.SentAt.Before(query.From.UTC()) {
			continue
		}
		if query.To != nil && !sample.SentAt.Before(query.To.UTC()) {
			continue
		}
		if query.Before != nil && !sample.SentAt.Before(query.Before.UTC()) {
			continue
		}
		matching = append(matching, sample)
	}
	if len(matching) > limit {
		matching = matching[len(matching)-limit:]
	}
	return append([]MetricsPayload(nil), matching...)
}

func (m *MetricsCollector) loadHistory() error {
	if err := os.MkdirAll(m.storageDir, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(m.storageDir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-m.retention)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		file, openErr := os.Open(filepath.Join(m.storageDir, entry.Name()))
		if openErr != nil {
			return openErr
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var sample MetricsPayload
			if json.Unmarshal(scanner.Bytes(), &sample) != nil || sample.SentAt.Before(cutoff) {
				continue
			}
			m.history = append(m.history, sample)
		}
		closeErr := file.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	m.sortHistory()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.compactStorageLocked()
}

func (m *MetricsCollector) sortHistory() {
	sort.Slice(m.history, func(i, j int) bool {
		return m.history[i].SentAt.Before(m.history[j].SentAt)
	})
}

func (m *MetricsCollector) pruneLocked(now time.Time) bool {
	cutoff := now.Add(-m.retention)
	first := 0
	for first < len(m.history) && m.history[first].SentAt.Before(cutoff) {
		first++
	}
	if first == 0 {
		return false
	}
	m.history = append([]MetricsPayload(nil), m.history[first:]...)
	return true
}

func (m *MetricsCollector) appendLocked(sample MetricsPayload) error {
	if m.storageDir == "" {
		return nil
	}
	line, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	path := filepath.Join(m.storageDir, sample.SentAt.UTC().Format(metricsDayFormat)+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}

func (m *MetricsCollector) compactStorageLocked() error {
	if m.storageDir == "" {
		return nil
	}
	entries, err := os.ReadDir(m.storageDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".jsonl" {
			if err := os.Remove(filepath.Join(m.storageDir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	byDay := make(map[string][]MetricsPayload)
	for _, sample := range m.history {
		day := sample.SentAt.UTC().Format(metricsDayFormat)
		byDay[day] = append(byDay[day], sample)
	}
	for day, samples := range byDay {
		var data bytes.Buffer
		encoder := json.NewEncoder(&data)
		for _, sample := range samples {
			if err := encoder.Encode(sample); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(m.storageDir, day+".jsonl"), data.Bytes(), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// virtualFilesystems are filesystem identifiers that never back user data;
// their partitions are skipped so the disk list stays limited to physical
// and network storage.
var virtualFilesystems = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "devfs": true, "udev": true,
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true,
	"overlay": true, "squashfs": true, "ramfs": true, "hugetlbfs": true,
	"mqueue": true, "shm": true, "devpts": true, "debugfs": true,
	"tracefs": true, "securityfs": true, "configfs": true, "fusectl": true,
	"pstore": true, "efivarfs": true, "autofs": true, "binfmt_misc": true,
	"rpc_pipefs": true, "nsfs": true, "bpf": true, "iso9660": true,
	"udf": true, "none": true, "map": true,
}

func reportablePartition(part disk.PartitionStat) bool {
	if virtualFilesystems[part.Fstype] {
		return false
	}
	return strings.HasPrefix(part.Device, "/dev/") ||
		strings.HasPrefix(part.Device, "//") || // SMB/CIFS share
		strings.Contains(part.Device, ":") // NFS host:/export, Windows C:
}

// collectDisks snapshots every reportable mounted filesystem (physical
// partitions and network mounts), root first, deduplicated by device so a
// filesystem mounted in several places (bind mounts, macOS synthesized
// snapshots) never counts twice.
func collectDisks() []DiskUsage {
	var disks []DiskUsage
	partitions, err := disk.Partitions(false)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, part := range partitions {
		if seen[part.Device] || !reportablePartition(part) {
			continue
		}
		seen[part.Device] = true
		usage, err := disk.Usage(part.Mountpoint)
		if err != nil {
			continue
		}
		disks = append(disks, DiskUsage{
			Mount:       part.Mountpoint,
			Filesystem:  part.Device,
			TotalKb:     int64(usage.Total / 1024),
			AvailableKb: int64(usage.Free / 1024),
		})
	}
	return disks
}

func (m *MetricsCollector) Collect() MetricsPayload {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	successes, failures := m.executor.Counts()
	var cpuPercent float64
	if values, err := cpu.Percent(0, false); err == nil && len(values) > 0 {
		cpuPercent = values[0]
	}
	var load1, load5, load15 float64
	if values, err := load.Avg(); err == nil {
		load1, load5, load15 = values.Load1, values.Load5, values.Load15
	}
	var memoryUsedPercent float64
	var memoryUsedBytes, memoryTotalBytes uint64
	if stats, err := mem.VirtualMemory(); err == nil {
		memoryUsedBytes, memoryUsedPercent = usedMemory(stats)
		memoryTotalBytes = stats.Total
	}
	var swapTotalKb, swapFreeKb int64
	if values, err := mem.SwapMemory(); err == nil {
		swapTotalKb = int64(values.Total / 1024)
		swapFreeKb = int64(values.Free / 1024)
	}
	var diskTotalKb, diskAvailableKb int64
	if usage, err := disk.Usage("/"); err == nil {
		diskTotalKb = int64(usage.Total / 1024)
		diskAvailableKb = int64(usage.Free / 1024)
	}
	disks := collectDisks()
	var netRxBytes, netTxBytes uint64
	if counters, err := net.IOCounters(true); err == nil {
		for _, counter := range counters {
			if counter.Name == "lo" || counter.Name == "lo0" {
				continue
			}
			netRxBytes += counter.BytesRecv
			netTxBytes += counter.BytesSent
		}
	}
	// System uptime, not daemon process uptime: the value must survive daemon
	// restarts and match the SSH collector's /proc/uptime semantics.
	var uptimeSeconds int64
	if uptime, err := host.Uptime(); err == nil {
		uptimeSeconds = int64(uptime)
	}
	return MetricsPayload{
		SentAt:             time.Now().UTC(),
		HostID:             m.hostID,
		UptimeSeconds:      uptimeSeconds,
		ProcessMemoryBytes: int64(stats.Alloc),
		CPUPercent:         cpuPercent,
		CPUCount:           runtime.NumCPU(),
		Load1:              load1,
		Load5:              load5,
		Load15:             load15,
		MemoryUsedPercent:  memoryUsedPercent,
		MemoryUsedBytes:    memoryUsedBytes,
		MemoryTotalBytes:   memoryTotalBytes,
		SwapTotalKb:        swapTotalKb,
		SwapFreeKb:         swapFreeKb,
		DiskTotalKb:        diskTotalKb,
		DiskAvailableKb:    diskAvailableKb,
		Disks:              disks,
		NetRxBytes:         netRxBytes,
		NetTxBytes:         netTxBytes,
		WebhookExecutions:  successes + failures,
		WebhookFailures:    failures,
	}
}

// usedMemory computes used bytes and the used percentage from gopsutil's
// memory snapshot using MemAvailable semantics (used = total - available), not
// gopsutil's legacy Total - Free - Buffers - Cached. The legacy formula credits
// every reclaimable page cache byte as free, so it under-reports usage on
// Linux and stays flat while a memory-hungry process evicts cache — exactly
// the values the MaidKit dashboard/overview/S-sidebar already show via the
// SSH collector (MemTotal - MemAvailable). Keeping both channels on the same
// semantics makes every MaidKit surface agree and lets the activity chart
// move when a process actually consumes memory.
func usedMemory(stats *mem.VirtualMemoryStat) (usedBytes uint64, usedPercent float64) {
	if stats.Total == 0 {
		return 0, 0
	}
	used := stats.Total - stats.Available
	percent := float64(used) / float64(stats.Total) * 100.0
	// Guard against gopsutil returning Available > Total on exotic hosts.
	if used > stats.Total {
		used = stats.Total
		percent = 100.0
	}
	return used, percent
}
