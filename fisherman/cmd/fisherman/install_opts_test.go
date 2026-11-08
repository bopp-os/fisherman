package main

import "testing"

func TestInstallMountOpts(t *testing.T) {
	if got := installMountOpts("btrfs"); got != "compress=zstd:1" {
		t.Errorf("btrfs: got %q", got)
	}
	for _, fs := range []string{"ext4", "xfs", "zfs", ""} {
		if got := installMountOpts(fs); got != "" {
			t.Errorf("%q: got %q, want no options", fs, got)
		}
	}
}
