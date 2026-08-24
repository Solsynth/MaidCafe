package daemon

import (
	"encoding/json"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
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
