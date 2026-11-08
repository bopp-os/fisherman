package post

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tuna-os/fisherman/internal/runner"
)

// Keeping the user's files across a reinstall ("keepFiles").
//
// The old root filesystem is mounted at the target, not formatted. Before bootc runs, PrepareKeptFiles moves the old
// homes and a few settings into <target>/.boppos-keep (a rename on the same filesystem, so nothing is copied) and
// deletes everything else of the old system. The caller self-binds that folder: bootc then sees a root that holds only
// a mount point, which its empty-root check accepts, and installs as on an empty disk. After CreateUser,
// RestoreKeptFiles moves the homes into the new stateroot's var/home, puts the old Wi-Fi profiles back and removes
// the folder.
//
// Where the old homes are, by kind of install (paths from the top of the root filesystem):
//   - bootc/ostree (an earlier BoppOS install): ostree/deploy/<stateroot>/var/home, or the same under @/ when the root
//     was a btrfs subvolume
//   - a btrfs distribution with a home subvolume (CachyOS's default): @home
//   - any other distribution: home
//
// Ownership: the home keeps its old UID/GID on disk. CreateUser's tmpfiles rule ("Z /var/home/<user>") hands the
// whole home to the new account on boot, so a different UID needs nothing here.

// KeepDirName is the folder at the top of the root filesystem that holds the kept files during the install.
const KeepDirName = ".boppos-keep"

// KeptFiles is what PrepareKeptFiles kept.
type KeptFiles struct {
	Dir         string   // <target>/.boppos-keep
	Homes       []string // names of the kept home folders, sorted
	Connections int      // NetworkManager profiles kept
	Source      string   // where the homes came from, relative to the top of the old root
}

// homeCandidates lists where an old install keeps its homes, most specific first.
func homeCandidates(top string) []string {
	var c []string
	for _, base := range []string{"", "@"} {
		deploy := filepath.Join(top, base, "ostree", "deploy")
		entries, _ := os.ReadDir(deploy)
		// "default" first: that is the stateroot fisherman and bootc use.
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name() == "default" && entries[j].Name() != "default" })
		for _, e := range entries {
			c = append(c, filepath.Join(base, "ostree", "deploy", e.Name(), "var", "home"))
		}
	}
	return append(c, "@home", "home", filepath.Join("@", "home"))
}

// oldEtcCandidates lists where an old install keeps its /etc: the newest ostree deployment's etc, then a plain etc.
func oldEtcCandidates(top string) []string {
	var c []string
	for _, base := range []string{"", "@"} {
		matches, _ := filepath.Glob(filepath.Join(top, base, "ostree", "deploy", "*", "deploy", "*", "etc"))
		sort.Slice(matches, func(i, j int) bool { return modTime(matches[i]) > modTime(matches[j]) })
		for _, m := range matches {
			rel, _ := filepath.Rel(top, m)
			c = append(c, rel)
		}
	}
	return append(c, "etc", filepath.Join("@", "etc"))
}

func modTime(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

// realHomes lists the folders in dir that look like user homes (not lost+found, not hidden).
func realHomes(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var homes []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "lost+found" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		homes = append(homes, e.Name())
	}
	sort.Strings(homes)
	return homes
}

// FindOldHomes returns where under top the old homes are and their names, or ErrNoOldHomes.
func FindOldHomes(top string) (string, []string, error) {
	for _, rel := range homeCandidates(top) {
		info, err := os.Lstat(filepath.Join(top, rel))
		if err != nil || !info.IsDir() {
			continue
		}
		if homes := realHomes(filepath.Join(top, rel)); len(homes) > 0 {
			return rel, homes, nil
		}
	}
	return "", nil, ErrNoOldHomes
}

// ErrNoOldHomes means the old root holds no user homes, so there is nothing to keep.
var ErrNoOldHomes = errors.New("no user homes found on the existing install")

// btrfsSetDefaultFn resets the btrfs default subvolume to the top level; tests stub it.
var btrfsSetDefaultFn = func(top string) error { return runner.Run("btrfs", "subvolume", "set-default", "5", top) }

// btrfsDeleteSubvolumesFn deletes the subvolumes under path (read-only snapshots included); tests stub it.
var btrfsDeleteSubvolumesFn = func(path string) error {
	return runner.Run("btrfs", "subvolume", "delete", "--recursive", path)
}

// PrepareKeptFiles moves the old homes and Wi-Fi profiles of the root filesystem mounted at top into
// top/.boppos-keep, then deletes everything else at top (lost+found stays). fstype is the root filesystem's type.
// Nothing is deleted until the homes are safely inside the keep folder; when no homes are found nothing changes and
// ErrNoOldHomes is returned. Nothing may be mounted below top yet.
func PrepareKeptFiles(top, fstype string) (*KeptFiles, error) {
	keep := filepath.Join(top, KeepDirName)
	k := &KeptFiles{Dir: keep}

	src, homes, err := FindOldHomes(top)
	if err != nil {
		// A keep folder from an interrupted earlier run already holds the homes: carry on from it.
		if prev := realHomes(filepath.Join(keep, "home")); len(prev) > 0 {
			k.Homes, k.Source = prev, filepath.Join(KeepDirName, "home")
			fmt.Fprintf(os.Stdout, "  continuing with the files kept by an earlier attempt: %s\n", strings.Join(prev, ", "))
			return k, wipeExcept(top, fstype)
		}
		return nil, fmt.Errorf("%w; nothing was erased", err)
	}
	k.Homes, k.Source = homes, src

	if err := os.MkdirAll(keep, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", keep, err)
	}
	// Settings first (copies), the homes last (a rename): a failure before the rename leaves the old system whole.
	for _, rel := range oldEtcCandidates(top) {
		etc := filepath.Join(top, rel)
		if info, err := os.Stat(etc); err != nil || !info.IsDir() {
			continue
		}
		n, err := keepConnections(etc, filepath.Join(keep, "NetworkManager"))
		if err != nil {
			return nil, fmt.Errorf("keeping the Wi-Fi profiles of %s: %w", rel, err)
		}
		k.Connections = n
		break
	}
	if _, err := os.Lstat(filepath.Join(keep, "home")); err == nil {
		return nil, fmt.Errorf("%s/home already exists; remove it or erase the disk", KeepDirName)
	}
	if err := os.Rename(filepath.Join(top, src), filepath.Join(keep, "home")); err != nil {
		return nil, fmt.Errorf("moving %s aside: %w", src, err)
	}
	fmt.Fprintf(os.Stdout, "  keeping %d home folder(s) from /%s: %s\n", len(homes), src, strings.Join(homes, ", "))
	if k.Connections > 0 {
		fmt.Fprintf(os.Stdout, "  keeping %d NetworkManager profile(s) of the old system\n", k.Connections)
	}
	if err := wipeExcept(top, fstype); err != nil {
		return nil, fmt.Errorf("the homes are safe in /%s, but removing the old system failed (installing again "+
			"with Keep my files carries on from there): %w", KeepDirName, err)
	}
	return k, nil
}

// keepConnections copies etc/NetworkManager/system-connections into dst (0700, files 0600).
func keepConnections(etc, dst string) (int, error) {
	srcDir := filepath.Join(etc, "NetworkManager", "system-connections")
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0, nil //nolint:nilerr // no profiles
	}
	n := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".nmconnection") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return n, err
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return n, err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// wipeExcept deletes everything at top except the keep folder and lost+found.
func wipeExcept(top, fstype string) error {
	if fstype == "btrfs" {
		// A default subvolume that is about to be deleted would make the new install unbootable (and btrfs refuses
		// to delete it).
		if err := btrfsSetDefaultFn(top); err != nil {
			return fmt.Errorf("resetting the btrfs default subvolume: %w", err)
		}
	}
	entries, err := os.ReadDir(top)
	if err != nil {
		return err
	}
	var removed []string
	for _, e := range entries {
		if e.Name() == KeepDirName || e.Name() == "lost+found" {
			continue
		}
		p := filepath.Join(top, e.Name())
		if fstype == "btrfs" && e.IsDir() {
			// Subvolumes and read-only snapshots (snapper) cannot all be removed file by file.
			_ = btrfsDeleteSubvolumesFn(p)
			if _, err := os.Lstat(p); os.IsNotExist(err) {
				removed = append(removed, e.Name())
				continue
			}
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("removing the old system's /%s: %w", e.Name(), err)
		}
		removed = append(removed, e.Name())
	}
	fmt.Fprintf(os.Stdout, "  removed the old system (%s)\n", strings.Join(removed, ", "))
	return nil
}

// RestoreKeptFiles moves the kept homes into the new install's stateroot var/home and writes the kept Wi-Fi
// profiles into the deployment's etc, then removes the keep folder. Call it after CreateUser and before
// CopyWiFiConnections and PreloadSteam, and only after the keep folder's self-bind is unmounted (a rename does not
// cross a mount point).
//
// The kept home whose name is username replaces the fresh one CreateUser made; any other kept home goes to
// /var/home/<name> as it is (its account is not recreated; the log says so). A home that cannot be moved stays in
// /.boppos-keep and the error says where.
func RestoreKeptFiles(target, username string, k *KeptFiles) error {
	if k == nil {
		return nil
	}
	if isComposeFsNative(target) {
		return fmt.Errorf("keeping files is not supported on composefs-native installs; they stay in /%s", KeepDirName)
	}
	homeDir := filepath.Join(target, "ostree", "deploy", "default", "var", "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return err
	}
	keptHome := filepath.Join(k.Dir, "home")
	var problems []string
	for _, name := range realHomes(keptHome) {
		src, dst := filepath.Join(keptHome, name), filepath.Join(homeDir, name)
		if name == username {
			// CreateUser's fresh home holds only /etc/skel; the kept one replaces it.
			if _, err := os.Lstat(dst); err == nil {
				if err := os.RemoveAll(dst); err != nil {
					problems = append(problems, fmt.Sprintf("%s: removing the new empty home: %v", name, err))
					continue
				}
			}
		} else if _, err := os.Lstat(dst); err == nil {
			problems = append(problems, fmt.Sprintf("%s: /var/home/%s already exists", name, name))
			continue
		}
		if err := moveTree(src, dst); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if name == username {
			fmt.Fprintf(os.Stdout, "  kept the files of %s in /var/home/%s\n", name, name)
		} else {
			fmt.Fprintf(os.Stdout, "  kept /var/home/%s; its account was not recreated (add it after the first boot)\n", name)
		}
	}
	if username != "" {
		if _, err := os.Stat(filepath.Join(keptHome, username)); err != nil && !contains(realHomes(homeDir), username) {
			fmt.Fprintf(os.Stdout, "  no kept home is named %s; the new account starts with an empty home\n", username)
		}
	}

	if n, err := restoreConnections(target, filepath.Join(k.Dir, "NetworkManager")); err != nil {
		problems = append(problems, fmt.Sprintf("Wi-Fi profiles: %v", err))
	} else if n > 0 {
		fmt.Fprintf(os.Stdout, "  restored %d NetworkManager profile(s) of the old system\n", n)
	}

	if len(problems) > 0 {
		return fmt.Errorf("some kept files stay in /%s: %s", KeepDirName, strings.Join(problems, "; "))
	}
	if err := os.RemoveAll(k.Dir); err != nil {
		return fmt.Errorf("removing /%s: %w", KeepDirName, err)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// moveTree renames src to dst; across btrfs subvolumes (EXDEV) it copies with reflinks, then removes src.
func moveTree(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || !strings.Contains(linkErr.Err.Error(), "cross-device") {
		return err
	}
	if err := runner.Run("cp", "-a", "--reflink=auto", src, dst); err != nil {
		_ = os.RemoveAll(dst)
		return fmt.Errorf("copying: %w", err)
	}
	return os.RemoveAll(src)
}

// restoreConnections writes the kept profiles into the deployment's etc as root 0600, leaving any profile already
// there alone.
func restoreConnections(target, keptDir string) (int, error) {
	entries, err := os.ReadDir(keptDir)
	if err != nil {
		return 0, nil //nolint:nilerr // none kept
	}
	etc, err := deployEtcDir(target)
	if err != nil {
		return 0, err
	}
	dst := filepath.Join(etc, "NetworkManager", "system-connections")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		path := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(keptDir, e.Name()))
		if err != nil {
			return n, err
		}
		if err := writeRootSecret(path, data); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
