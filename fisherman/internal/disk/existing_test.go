package disk_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/fisherman/internal/disk"
)

const esp = `"c12a7328-f81f-11d2-ba4b-00a0c93ec93b"`

func lsblkJSON(parts ...string) []byte {
	return []byte(`{"blockdevices":[{"name":"/dev/nvme0n1","type":"disk","children":[` + strings.Join(parts, ",") + `]}]}`)
}

func part(name, fstype, parttype, label string, size int) string {
	q := func(s string) string {
		if s == "" {
			return "null"
		}
		if strings.HasPrefix(s, `"`) {
			return s
		}
		return `"` + s + `"`
	}
	return `{"name":"` + name + `","type":"part","fstype":` + q(fstype) + `,"parttype":` + q(parttype) +
		`,"label":` + q(label) + `,"partlabel":null,"size":` + strings.Repeat("1", 1) + strings.Repeat("0", size) + `}`
}

func TestParseExistingLayout(t *testing.T) {
	linuxRoot := "4f68bce3-e8cd-4db1-96e7-fbcaf984b709"
	cases := []struct {
		name  string
		parts []string
		want  disk.ExistingLayout
	}{
		{"fisherman systemd-boot, encrypted", []string{
			part("/dev/nvme0n1p1", "vfat", esp, "", 9),
			part("/dev/nvme0n1p2", "crypto_LUKS", linuxRoot, "", 11),
		}, disk.ExistingLayout{EFI: "/dev/nvme0n1p1", Root: "/dev/nvme0n1p2", RootLUKS: true}},
		{"fisherman grub, encrypted", []string{
			part("/dev/nvme0n1p1", "vfat", esp, "", 9),
			part("/dev/nvme0n1p2", "ext4", "0fc63daf-8483-4772-8e79-3d69d8477de4", "boot", 9),
			part("/dev/nvme0n1p3", "crypto_LUKS", linuxRoot, "", 11),
		}, disk.ExistingLayout{EFI: "/dev/nvme0n1p1", Boot: "/dev/nvme0n1p2", Root: "/dev/nvme0n1p3", RootLUKS: true}},
		{"plain CachyOS btrfs with swap", []string{
			part("/dev/nvme0n1p1", "vfat", esp, "", 9),
			part("/dev/nvme0n1p2", "btrfs", linuxRoot, "", 11),
			part("/dev/nvme0n1p3", "swap", "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f", "", 10),
		}, disk.ExistingLayout{EFI: "/dev/nvme0n1p1", Root: "/dev/nvme0n1p2", RootFS: "btrfs"}},
		{"largest plain partition wins", []string{
			part("/dev/nvme0n1p1", "vfat", esp, "", 9),
			part("/dev/nvme0n1p2", "ext4", linuxRoot, "", 10),
			part("/dev/nvme0n1p3", "xfs", linuxRoot, "", 11),
		}, disk.ExistingLayout{EFI: "/dev/nvme0n1p1", Root: "/dev/nvme0n1p3", RootFS: "xfs"}},
	}
	for _, c := range cases {
		got, err := disk.ParseExistingLayout(lsblkJSON(c.parts...))
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v %v, want %+v", c.name, got, err, c.want)
		}
	}
}

func TestParseExistingLayoutRefusesWhatItCannotKeep(t *testing.T) {
	for name, parts := range map[string][]string{
		"no ESP":       {part("/dev/nvme0n1p1", "crypto_LUKS", "", "", 11)},
		"two LUKS":     {part("/dev/nvme0n1p1", "vfat", esp, "", 9), part("/dev/nvme0n1p2", "crypto_LUKS", "", "", 11), part("/dev/nvme0n1p3", "crypto_LUKS", "", "", 11)},
		"Windows only": {part("/dev/nvme0n1p1", "vfat", esp, "", 9), part("/dev/nvme0n1p2", "ntfs", "", "", 11)},
		"empty disk":   {},
	} {
		if _, err := disk.ParseExistingLayout(lsblkJSON(parts...)); !errors.Is(err, disk.ErrNoExistingInstall) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// A real LUKS2 header in a disk image file: blkid, which FindExistingLayout's lsblk and FSType rely on, reports it
// as crypto_LUKS. (Partitioned images need a loop device, which this test does not use.)
func TestFSTypeSeesARealLUKSContainer(t *testing.T) {
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		t.Skip("cryptsetup not available")
	}
	if _, err := exec.LookPath("blkid"); err != nil {
		t.Skip("blkid not available")
	}
	img := filepath.Join(t.TempDir(), "root.img")
	if err := os.WriteFile(img, make([]byte, 32<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cryptsetup", "luksFormat", "--batch-mode", "--type=luks2", "--pbkdf=pbkdf2",
		"--pbkdf-force-iterations=1000", "--disable-locks", "--key-file=-", img)
	cmd.Stdin = strings.NewReader("test passphrase")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("luksFormat on a file is not possible here: %v %s", err, out)
	}
	if got := disk.FSType(img); got != "crypto_LUKS" {
		t.Errorf("FSType = %q, want crypto_LUKS", got)
	}
}
