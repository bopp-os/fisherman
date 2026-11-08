package disk

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tuna-os/fisherman/internal/runner"
)

// Finding an earlier install to reinstall over without erasing the user's files ("keepFiles").
//
// fisherman's own layouts are: p1 ESP + p2 root (systemd-boot), or p1 ESP + p2 ext4 "boot" + p3 root (grub); the
// root is LUKS when the install was encrypted. Other distributions' installers (CachyOS's Calamares, for one) use the
// same shape: an ESP and one Linux root. FindExistingLayout reads the disk's partitions and picks those out by type,
// without assuming partition numbers. Nothing is changed on the disk.

// ESPPartType is the GPT type GUID of an EFI system partition.
const ESPPartType = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"

// ExistingLayout is what a keep-files install reuses.
type ExistingLayout struct {
	EFI      string // the ESP, reformatted
	Boot     string // the separate ext4 /boot of a grub layout, reformatted; "" when there is none
	Root     string // the partition holding the root filesystem (or its LUKS container); never formatted
	RootLUKS bool   // Root is a LUKS container
	RootFS   string // the root filesystem type when it is not encrypted ("" for LUKS until it is opened)
}

type lsblkPart struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	FSType   *string     `json:"fstype"`
	PartType *string     `json:"parttype"`
	Label    *string     `json:"label"`
	PartName *string     `json:"partlabel"`
	Size     json.Number `json:"size"`
	Children []lsblkPart `json:"children"`
}

func strp(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*s))
}

// ErrNoExistingInstall means the disk does not look like an install that can be kept.
var ErrNoExistingInstall = errors.New("no existing install found")

// keptRootFilesystems are the root filesystems a keep-files install can mount and keep.
var keptRootFilesystems = map[string]bool{"crypto_luks": true, "ext4": true, "xfs": true, "btrfs": true}

// PartInfo is one partition as lsblk reports it.
type PartInfo struct {
	Name, FSType, PartType, Label, PartLabel string
	Size                                     int64
}

// ParseExistingLayout picks the ESP, an optional ext4 /boot and the root out of `lsblk -J -b -p -o
// NAME,TYPE,FSTYPE,PARTTYPE,LABEL,PARTLABEL,SIZE <disk>` output (see LayoutFromParts).
func ParseExistingLayout(data []byte) (ExistingLayout, error) {
	var out struct {
		Blockdevices []lsblkPart `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ExistingLayout{}, fmt.Errorf("reading lsblk output: %w", err)
	}
	if len(out.Blockdevices) != 1 {
		return ExistingLayout{}, fmt.Errorf("lsblk listed %d disks, want 1", len(out.Blockdevices))
	}
	var parts []PartInfo
	for _, p := range out.Blockdevices[0].Children {
		if p.Type != "part" {
			continue
		}
		size, _ := p.Size.Int64()
		parts = append(parts, PartInfo{p.Name, strp(p.FSType), strp(p.PartType), strp(p.Label), strp(p.PartName), size})
	}
	return LayoutFromParts(parts)
}

// LayoutFromParts picks the ESP, an optional ext4 /boot and the root of an existing install. The root is the one
// partition that is LUKS, or failing that the largest ext4/xfs/btrfs partition that is not /boot. More than one LUKS
// partition, or no ESP, is refused: the installer cannot tell which one to keep.
func LayoutFromParts(parts []PartInfo) (ExistingLayout, error) {
	var l ExistingLayout
	var luks []string
	var plainRoot, plainFS string
	var plainSize int64
	for _, p := range parts {
		fs, pt := strings.ToLower(p.FSType), strings.ToLower(p.PartType)
		switch {
		case pt == ESPPartType:
			if l.EFI == "" {
				l.EFI = p.Name
			}
		case fs == "crypto_luks":
			luks = append(luks, p.Name)
		case fs == "ext4" && (strings.EqualFold(p.Label, "boot") || strings.EqualFold(p.PartLabel, "boot")):
			l.Boot = p.Name
		case keptRootFilesystems[fs]:
			if plainRoot == "" || p.Size > plainSize {
				plainRoot, plainSize, plainFS = p.Name, p.Size, fs
			}
		}
	}
	switch {
	case l.EFI == "":
		return ExistingLayout{}, fmt.Errorf("%w: the disk has no EFI system partition", ErrNoExistingInstall)
	case len(luks) > 1:
		return ExistingLayout{}, fmt.Errorf("%w: the disk has %d encrypted partitions (%s); only one can be kept",
			ErrNoExistingInstall, len(luks), strings.Join(luks, ", "))
	case len(luks) == 1:
		l.Root, l.RootLUKS = luks[0], true
	case plainRoot != "":
		l.Root, l.RootFS = plainRoot, plainFS
	default:
		return ExistingLayout{}, fmt.Errorf("%w: the disk has no Linux root partition", ErrNoExistingInstall)
	}
	return l, nil
}

// FindExistingLayout reads disk's partitions (see ParseExistingLayout).
func FindExistingLayout(disk string) (ExistingLayout, error) {
	out, err := runner.Output("lsblk", "-J", "-b", "-p", "-o", "NAME,TYPE,FSTYPE,PARTTYPE,LABEL,PARTLABEL,SIZE", disk)
	if err != nil {
		return ExistingLayout{}, fmt.Errorf("listing partitions of %s: %w", disk, err)
	}
	return ParseExistingLayout(out)
}

// FSType returns the filesystem type blkid sees on dev ("" when it sees none).
func FSType(dev string) string {
	out, err := runner.Output("blkid", "-s", "TYPE", "-o", "value", dev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ReleaseDisk unmounts every partition of disk and closes the device-mapper devices on it (a LUKS container the live
// desktop unlocked, for one), without changing the disk. A keep-files install calls it before it unlocks the root.
func ReleaseDisk(disk string) error {
	return unmountAll(disk)
}
