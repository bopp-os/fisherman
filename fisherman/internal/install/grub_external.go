package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/tuna-os/fisherman/internal/progress"
	"github.com/tuna-os/fisherman/internal/runner"
)

// bootEntry is one ostree boot loader entry: a BLS file under /boot/loader/entries.
type bootEntry struct {
	title   string
	version string
	linux   string
	initrd  []string
	options string
}

// parseBootEntry reads the fields grub needs from the text of a BLS entry.
func parseBootEntry(data string) bootEntry {
	var e bootEntry
	for _, line := range strings.Split(data, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "title":
			e.title = val
		case "version":
			e.version = val
		case "linux":
			e.linux = val
		case "initrd":
			e.initrd = append(e.initrd, val)
		case "options":
			e.options = val
		}
	}
	return e
}

// readBootEntries returns the entries in dir, newest first. ostree numbers
// versions upward, so the highest version is the newest deployment.
func readBootEntries(dir string) ([]bootEntry, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []bootEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".conf") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			continue
		}
		if e := parseBootEntry(string(data)); e.linux != "" {
			entries = append(entries, e)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, errA := strconv.Atoi(entries[i].version)
		b, errB := strconv.Atoi(entries[j].version)
		if errA == nil && errB == nil {
			return a > b
		}
		return entries[i].version > entries[j].version
	})
	return entries, nil
}

// renderGrubCfg writes a grub.cfg with one menu entry per boot entry, newest
// first (the default). bootUUID is the filesystem UUID of the partition that
// holds /boot, which is where the entries' linux and initrd paths are rooted.
func renderGrubCfg(entries []bootEntry, bootUUID string) string {
	var b strings.Builder
	b.WriteString("set default=0\nset timeout=5\ninsmod part_gpt\ninsmod ext2\ninsmod btrfs\n")
	for _, e := range entries {
		title := strings.ReplaceAll(e.title, "'", "")
		if title == "" {
			title = "Linux"
		}
		fmt.Fprintf(&b, "menuentry '%s' {\n", title)
		fmt.Fprintf(&b, "    search --no-floppy --fs-uuid --set=root %s\n", bootUUID)
		fmt.Fprintf(&b, "    linux %s %s\n", e.linux, e.options)
		if len(e.initrd) > 0 {
			fmt.Fprintf(&b, "    initrd %s\n", strings.Join(e.initrd, " "))
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// setINIValue sets key=value inside [section] of an ini-style file (the ostree
// repo config), adding the key or the section when missing.
func setINIValue(content, section, key, value string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	header := "[" + section + "]"
	inSection, sectionEnd, found := false, -1, false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			if inSection && sectionEnd < 0 {
				sectionEnd = i
			}
			inSection = t == header
			continue
		}
		if inSection {
			k, _, ok := strings.Cut(t, "=")
			if ok && strings.TrimSpace(k) == key {
				lines[i] = key + "=" + value
				found = true
			}
		}
	}
	if found {
		return strings.Join(lines, "\n") + "\n"
	}
	newLine := key + "=" + value
	switch {
	case sectionEnd >= 0:
		lines = append(lines[:sectionEnd], append([]string{newLine}, lines[sectionEnd:]...)...)
	case inSection:
		lines = append(lines, newLine)
	default:
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		lines = append(lines, header, newLine)
	}
	return strings.Join(lines, "\n") + "\n"
}

// grubEFITarget is grub-install's --target for this architecture.
func grubEFITarget() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-efi"
	case "arm64":
		return "arm64-efi"
	}
	return ""
}

// InstallGrubExternal installs grub for an ostree system that bootc installed with
// --bootloader none (bootupd is missing from the image): grub-install onto the
// ESP, a first grub.cfg built from ostree's boot entries, and ostree's
// sysroot.bootloader set to grub2 so that every later upgrade regenerates
// grub.cfg through the image's grub-mkconfig and 15_ostree.
//
// target is the mounted root with /boot and /boot/efi mounted under it.
func InstallGrubExternal(target, bootloaderID string) error {
	grubTarget := grubEFITarget()
	if grubTarget == "" {
		return fmt.Errorf("no grub EFI target for %s", runtime.GOARCH)
	}
	bootDir := filepath.Join(target, "boot")
	common := []string{"--target=" + grubTarget, "--efi-directory=" + filepath.Join(bootDir, "efi"), "--boot-directory=" + bootDir}

	progress.Info("Installing GRUB")
	// The fallback loader (EFI/BOOT) first: it boots on firmware with no entry, and on a disk moved to another machine.
	if err := runner.Run("grub-install", append(append([]string{}, common...), "--removable")...); err != nil {
		return fmt.Errorf("grub-install --removable: %w", err)
	}
	if bootloaderID == "" {
		bootloaderID = "bootc"
	}
	if err := runner.Run("grub-install", append(append([]string{}, common...), "--bootloader-id="+bootloaderID)...); err != nil {
		// VMs and some firmware refuse NVRAM writes; the fallback loader above still boots.
		progress.Info(fmt.Sprintf("Warning: could not add a firmware boot entry: %v", err))
	}

	entries, err := readBootEntries(filepath.Join(bootDir, "loader", "entries"))
	if err != nil {
		return fmt.Errorf("reading ostree boot entries: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no ostree boot entries under %s/loader/entries", bootDir)
	}
	uuidOut, err := runner.Output("findmnt", "-no", "UUID", "-T", bootDir)
	if err != nil || strings.TrimSpace(string(uuidOut)) == "" {
		return fmt.Errorf("finding the UUID of the filesystem holding %s: %v", bootDir, err)
	}
	cfg := renderGrubCfg(entries, strings.TrimSpace(string(uuidOut)))
	grubDir := filepath.Join(bootDir, "grub")
	if err := runner.Run("mkdir", "-p", grubDir); err != nil {
		return fmt.Errorf("mkdir %s: %w", grubDir, err)
	}
	cfgPath := filepath.Join(grubDir, "grub.cfg")
	if err := runner.RunWithStdin(strings.NewReader(cfg), "tee", cfgPath); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}

	// bootc wrote sysroot.bootloader=none; let ostree manage grub.cfg from now on.
	repoConfig := filepath.Join(target, "ostree", "repo", "config")
	raw, err := runner.Output("cat", repoConfig)
	if err != nil {
		return fmt.Errorf("reading %s: %w", repoConfig, err)
	}
	updated := setINIValue(string(raw), "sysroot", "bootloader", "grub2")
	if err := runner.RunWithStdin(strings.NewReader(updated), "tee", repoConfig); err != nil {
		return fmt.Errorf("write %s: %w", repoConfig, err)
	}
	return nil
}
