package post

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func topEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// oldBootcRoot is the top of an earlier BoppOS (ostree) root filesystem.
func oldBootcRoot(t *testing.T) string {
	t.Helper()
	top := t.TempDir()
	writeFile(t, filepath.Join(top, "ostree/deploy/default/var/home/ripps/.local/share/Steam/steam.sh"), "steam")
	writeFile(t, filepath.Join(top, "ostree/deploy/default/var/home/ripps/Documents/notes.txt"), "mine")
	writeFile(t, filepath.Join(top, "ostree/deploy/default/var/home/guest/.bashrc"), "guest")
	writeFile(t, filepath.Join(top, "ostree/deploy/default/deploy/abc.0/etc/NetworkManager/system-connections/Home.nmconnection"),
		"[connection]\nid=Home\nuuid=1111\ntype=wifi\n[wifi-security]\npsk=secret\n")
	writeFile(t, filepath.Join(top, "ostree/repo/objects/00/x.file"), "old os")
	writeFile(t, filepath.Join(top, "boot/loader/entries/old.conf"), "old")
	os.MkdirAll(filepath.Join(top, "lost+found"), 0o700)
	return top
}

func stubBtrfs(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	oldSet, oldDel := btrfsSetDefaultFn, btrfsDeleteSubvolumesFn
	btrfsSetDefaultFn = func(top string) error { calls = append(calls, "set-default "+top); return nil }
	btrfsDeleteSubvolumesFn = func(p string) error {
		calls = append(calls, "delete "+filepath.Base(p))
		return os.RemoveAll(p)
	}
	t.Cleanup(func() { btrfsSetDefaultFn, btrfsDeleteSubvolumesFn = oldSet, oldDel })
	return &calls
}

func TestPrepareKeptFilesKeepsTheHomesOfAnEarlierBootcInstall(t *testing.T) {
	top := oldBootcRoot(t)
	var k *KeptFiles
	var err error
	out := captureStdout(t, func() { k, err = PrepareKeptFiles(top, "xfs") })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(k.Homes, ",") != "guest,ripps" || k.Connections != 1 {
		t.Errorf("kept %+v", k)
	}
	if got := strings.Join(topEntries(t, top), ","); got != ".boppos-keep,lost+found" {
		t.Errorf("left at the top: %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(top, KeepDirName, "home/ripps/Documents/notes.txt")); string(b) != "mine" {
		t.Error("home not kept")
	}
	if _, err := os.Stat(filepath.Join(top, KeepDirName, "NetworkManager/Home.nmconnection")); err != nil {
		t.Error("Wi-Fi profile not kept:", err)
	}
	if strings.Contains(out, "secret") {
		t.Error("the log shows a Wi-Fi password")
	}
}

func TestPrepareKeptFilesWithoutHomesChangesNothing(t *testing.T) {
	top := t.TempDir()
	writeFile(t, filepath.Join(top, "ostree/repo/config"), "x")
	os.MkdirAll(filepath.Join(top, "ostree/deploy/default/var/home"), 0o755)
	_, err := PrepareKeptFiles(top, "ext4")
	if !errors.Is(err, ErrNoOldHomes) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(top, "ostree/repo/config")); err != nil {
		t.Error("deleted the old system although nothing was kept")
	}
	if _, err := os.Stat(filepath.Join(top, KeepDirName)); err == nil {
		t.Error("made a keep folder for nothing")
	}
}

func TestPrepareKeptFilesKeepsACachyOSBtrfsHomeSubvolume(t *testing.T) {
	calls := stubBtrfs(t)
	top := t.TempDir()
	writeFile(t, filepath.Join(top, "@home/ripps/.config/x"), "cfg")
	writeFile(t, filepath.Join(top, "@/etc/NetworkManager/system-connections/Cafe.nmconnection"), "[connection]\nid=Cafe\n")
	writeFile(t, filepath.Join(top, "@/usr/bin/bash"), "elf")
	writeFile(t, filepath.Join(top, "@snapshots/1/snapshot/usr/bin/bash"), "elf")

	k, err := PrepareKeptFiles(top, "btrfs")
	if err != nil {
		t.Fatal(err)
	}
	if k.Source != "@home" || strings.Join(k.Homes, ",") != "ripps" || k.Connections != 1 {
		t.Errorf("kept %+v", k)
	}
	if got := strings.Join(topEntries(t, top), ","); got != ".boppos-keep" {
		t.Errorf("left at the top: %s", got)
	}
	if (*calls)[0] != "set-default "+top {
		t.Errorf("btrfs calls %v: the default subvolume must be reset first", *calls)
	}
}

func TestPrepareKeptFilesContinuesAnInterruptedRun(t *testing.T) {
	top := t.TempDir()
	writeFile(t, filepath.Join(top, KeepDirName, "home/ripps/a"), "a")
	writeFile(t, filepath.Join(top, "ostree/repo/config"), "half-installed")
	k, err := PrepareKeptFiles(top, "xfs")
	if err != nil || strings.Join(k.Homes, ",") != "ripps" {
		t.Fatalf("%+v %v", k, err)
	}
	if got := strings.Join(topEntries(t, top), ","); got != ".boppos-keep" {
		t.Errorf("left at the top: %s", got)
	}
}

func TestRestoreKeptFilesMovesTheHomesIntoTheNewInstall(t *testing.T) {
	recordChown(t)
	old := oldBootcRoot(t)
	k, err := PrepareKeptFiles(old, "xfs")
	if err != nil {
		t.Fatal(err)
	}
	// The new install lands on the same filesystem: reuse the folder as the target.
	target, deployDir := fakeOstreeTarget(t)
	if err := os.Rename(k.Dir, filepath.Join(target, KeepDirName)); err != nil {
		t.Fatal(err)
	}
	k.Dir = filepath.Join(target, KeepDirName)
	home := filepath.Join(target, "ostree/deploy/default/var/home")
	writeFile(t, filepath.Join(home, "ripps/.bashrc"), "skel") // what CreateUser made
	nm := filepath.Join(deployDir, "etc/NetworkManager/system-connections")
	writeFile(t, filepath.Join(nm, "Image.nmconnection"), "[connection]\nid=Image\n")

	out := captureStdout(t, func() {
		if err := RestoreKeptFiles(target, "ripps", k); err != nil {
			t.Fatal(err)
		}
	})
	if b, _ := os.ReadFile(filepath.Join(home, "ripps/Documents/notes.txt")); string(b) != "mine" {
		t.Error("ripps's files are not in the new home")
	}
	if _, err := os.Stat(filepath.Join(home, "ripps/.bashrc")); err == nil {
		t.Error("the fresh skeleton home was merged in instead of replaced")
	}
	if _, err := os.Stat(filepath.Join(home, "ripps/.local/share/Steam/steam.sh")); err != nil {
		t.Error("Steam folder not kept, so the preload would run again:", err)
	}
	if _, err := os.Stat(filepath.Join(home, "guest/.bashrc")); err != nil {
		t.Error("the other home was not kept:", err)
	}
	if !strings.Contains(out, "guest; its account was not recreated") {
		t.Errorf("log does not say the guest account is gone: %s", out)
	}
	info, err := os.Stat(filepath.Join(nm, "Home.nmconnection"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("old Wi-Fi profile not restored as 0600: %v %v", info, err)
	}
	if _, err := os.Stat(k.Dir); err == nil {
		t.Error("the keep folder was left behind")
	}
}

func TestRestoreKeptFilesLeavesAHomeItCannotPlace(t *testing.T) {
	target, _ := fakeOstreeTarget(t)
	keep := filepath.Join(target, KeepDirName)
	writeFile(t, filepath.Join(keep, "home/guest/a"), "kept")
	writeFile(t, filepath.Join(target, "ostree/deploy/default/var/home/guest/b"), "new") // already there
	err := RestoreKeptFiles(target, "ripps", &KeptFiles{Dir: keep})
	if err == nil || !strings.Contains(err.Error(), ".boppos-keep") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(keep, "home/guest/a")); err != nil {
		t.Error("the kept home was lost:", err)
	}
}

func TestCopyWiFiConnectionsLeavesAKeptProfileAlone(t *testing.T) {
	target, deployDir := fakeOstreeTarget(t)
	recordChown(t)
	dst := filepath.Join(deployDir, "etc/NetworkManager/system-connections")
	writeFile(t, filepath.Join(dst, "Old name.nmconnection"), "[connection]\nid=Home Wi-Fi\nuuid=0b8f6a1e-8f1c-4c55-9f83-2b4c6d0f7a11\ntype=wifi\n[wifi-security]\npsk=old\n")
	liveConnections(t, map[string]string{
		"Home Wi-Fi.nmconnection": wifiProfile, // same uuid
		"Cafe.nmconnection":       "[connection]\nid=Cafe\ntype=wifi\n",
	})
	out := captureStdout(t, func() {
		if err := CopyWiFiConnections(target); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(filepath.Join(dst, "Home Wi-Fi.nmconnection")); err == nil {
		t.Error("copied a profile the installed system already has")
	}
	if _, err := os.Stat(filepath.Join(dst, "Cafe.nmconnection")); err != nil {
		t.Error("new profile not copied:", err)
	}
	if !strings.Contains(out, "already has it") {
		t.Errorf("log: %s", out)
	}
}
