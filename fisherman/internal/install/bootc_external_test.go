package install_test

import (
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/install"
)

// grub-external asks bootc for no bootloader (fisherman installs grub itself) and, unlike
// systemd, does not ask for the composefs backend.
func TestBuildBootcArgsGrubExternal(t *testing.T) {
	args := strings.Join(install.BuildBootcArgs(install.Options{Bootloader: "grub-external"}, "", "/target"), " ")
	if !strings.Contains(args, "--bootloader none") {
		t.Errorf("want --bootloader none, got %q", args)
	}
	if strings.Contains(args, "--composefs-backend") {
		t.Errorf("grub-external must stay on the ostree backend, got %q", args)
	}
}
