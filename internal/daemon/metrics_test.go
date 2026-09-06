package daemon

import (
	"encoding/json"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

func TestReportablePartition(t *testing.T) {
	cases := []struct {
		name string
		part disk.PartitionStat
		want bool
	}{
		{
			name: "physical partition",
			part: disk.PartitionStat{Device: "/dev/vda1", Mountpoint: "/", Fstype: "xfs"},
			want: true,
		},
		{
			name: "physical partition with spaces",
			part: disk.PartitionStat{Device: "/dev/sdb1", Mountpoint: "/mnt/My Disk", Fstype: "ext4"},
			want: true,
		},
		{
			name: "network NFS mount",
			part: disk.PartitionStat{Device: "10.0.0.5:/export", Mountpoint: "/mnt/nfs", Fstype: "nfs4"},
			want: true,
		},
		{
			name: "network CIFS mount",
			part: disk.PartitionStat{Device: "//nas/share", Mountpoint: "/mnt/smb", Fstype: "cifs"},
			want: true,
		},
		{
			name: "windows drive letter",
			part: disk.PartitionStat{Device: "D:", Mountpoint: "D:", Fstype: "ntfs"},
			want: true,
		},
		{
			name: "tmpfs",
			part: disk.PartitionStat{Device: "tmpfs", Mountpoint: "/dev/shm", Fstype: "tmpfs"},
			want: false,
		},
		{
			name: "overlay",
			part: disk.PartitionStat{Device: "overlay", Mountpoint: "/", Fstype: "overlay"},
			want: false,
		},
		{
			name: "devfs",
			part: disk.PartitionStat{Device: "devfs", Mountpoint: "/dev", Fstype: "devfs"},
			want: false,
		},
		{
			name: "autofs map",
			part: disk.PartitionStat{Device: "map auto_home", Mountpoint: "/System/Volumes/Data/home", Fstype: "autofs"},
			want: false,
		},
		{
			name: "optical drive",
			part: disk.PartitionStat{Device: "/dev/sr0", Mountpoint: "/media/cdrom", Fstype: "iso9660"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportablePartition(tc.part); got != tc.want {
				t.Fatalf("reportablePartition(%+v) = %v, want %v", tc.part, got, tc.want)
			}
		})
	}
}

func TestDiskUsageJSONShape(t *testing.T) {
	payload := MetricsPayload{
		Disks: []DiskUsage{
			{Mount: "/", Filesystem: "/dev/vda1", TotalKb: 102400, AvailableKb: 51200},
			{Mount: "/data", Filesystem: "/dev/vdb1", TotalKb: 516555776, AvailableKb: 412876800},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Disks []DiskUsage `json:"disks"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Disks) != 2 || decoded.Disks[0].Mount != "/" ||
		decoded.Disks[1].AvailableKb != 412876800 {
		t.Fatalf("disks round-trip = %+v", decoded.Disks)
	}
}

// TestUsedMemoryMemAvailable verifies the daemon reports used memory with
// MemAvailable semantics (total - available) rather than gopsutil's legacy
// Total - Free - Buffers - Cached. The legacy formula credits reclaimable
// page cache as free, which under-reports usage and stays flat while a
// memory-hungry process evicts cache — the values the SSH collector and the
// other MaidKit surfaces already use.
func TestUsedMemoryMemAvailable(t *testing.T) {
	cases := []struct {
		name      string
		total     uint64
		avail     uint64
		wantBytes uint64
		wantPct   float64
	}{
		{
			// Mirrors the issue: 30.9GB total, ~19.6GB available
			// (MemAvailable); the legacy formula reports ~9% while the
			// real used is ~37%.
			name:      "linux memavailable used",
			total:     30 << 30,
			avail:     19 << 30,
			wantBytes: 11 << 30,
			wantPct:   11.0 / 30.0 * 100.0,
		},
		{
			name:      "nearly idle",
			total:     16 << 30,
			avail:     15 << 30,
			wantBytes: 1 << 30,
			wantPct:   6.25,
		},
		{
			name:      "all memory used",
			total:     8 << 30,
			avail:     0,
			wantBytes: 8 << 30,
			wantPct:   100,
		},
		{
			name:      "zero total is safe",
			total:     0,
			avail:     0,
			wantBytes: 0,
			wantPct:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			used, pct := usedMemory(&mem.VirtualMemoryStat{
				Total:     tc.total,
				Available: tc.avail,
			})
			if used != tc.wantBytes {
				t.Fatalf("used bytes = %d, want %d", used, tc.wantBytes)
			}
			if pct != tc.wantPct {
				t.Fatalf("used percent = %v, want %v", pct, tc.wantPct)
			}
		})
	}
}
