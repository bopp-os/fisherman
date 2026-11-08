package post

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tuna-os/fisherman/internal/runner"
)

// Unpacking the preloaded Steam client.
//
// Storing the packages alone is not enough: Steam's first start still installs them, and Game Mode shows a black
// screen until ubuntu12_32/steamui.so exists. PreloadSteam therefore also unpacks every package into the Steam folder,
// the way Steam's bootstrapper does, writing steamui.so last so a failed unpack never leaves a client that looks
// complete. The same method as boppos-tools' boppos/steam_client.py (the deck first-boot step), so the two agree.
//
// No ".installed" marker is written: Steam checks the unpacked files against the manifest on its first start and
// sets its own file modes, but finds nothing to download.

// steamDeckManifestName is the manifest of the steamdeck_stable branch, which Steam uses when it starts with
// -steamdeck (the CachyOS handheld images always do). Steam keeps it in package/ under this name.
const steamDeckManifestName = "steam_client_steamdeck_stable_ubuntu12"

// xzCommand decompresses the LZMA stream inside a VZ package. Tests may point it elsewhere.
var xzCommand = "xz"

// unvz turns one of Valve's VZ files into the zip inside it. A VZ file is "VZa", a 4-byte time stamp, the 5 bytes of
// LZMA properties, a raw LZMA stream, then the CRC-32 and size of the result and "zv". xz reads it as a .lzma
// (LZMA_Alone) stream once the properties are followed by the 8-byte size.
func unvz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	head := make([]byte, 12)
	tail := make([]byte, 10)
	if info.Size() < 22 {
		return fmt.Errorf("%s is not a VZ file", filepath.Base(src))
	}
	if _, err := f.ReadAt(head, 0); err != nil {
		return err
	}
	if _, err := f.ReadAt(tail, info.Size()-10); err != nil {
		return err
	}
	if !bytes.Equal(head[:3], []byte("VZa")) || !bytes.Equal(tail[8:], []byte("zv")) {
		return fmt.Errorf("%s is not a VZ file", filepath.Base(src))
	}
	wantCRC := binary.LittleEndian.Uint32(tail[0:4])
	size := binary.LittleEndian.Uint32(tail[4:8])

	alone := make([]byte, 13)
	copy(alone, head[7:12])
	binary.LittleEndian.PutUint64(alone[5:], uint64(size))

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	h := crc32.NewIEEE()
	cw := &countWriter{w: io.MultiWriter(out, h)}
	var stderr bytes.Buffer
	cmd := runner.HostCommand(xzCommand, "--decompress", "--stdout", "--format=lzma", "--single-stream")
	cmd.Stdin = io.MultiReader(bytes.NewReader(alone), io.NewSectionReader(f, 12, info.Size()-22))
	cmd.Stdout = cw
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: xz: %v %s", filepath.Base(src), err, strings.TrimSpace(stderr.String()))
	}
	if cw.n != int64(size) || h.Sum32() != wantCRC {
		return fmt.Errorf("%w: %s unpacked to data that does not match its CRC", errSteamChecksum, filepath.Base(src))
	}
	return out.Close()
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

var windowsDriveRe = regexp.MustCompile(`^[A-Za-z]:`)

// steamMemberPath is where a zip member goes under dest, or "" when its name would leave dest. Some of Valve's zips
// were made on Windows and separate with backslashes, so both count as separators.
func steamMemberPath(dest, name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || windowsDriveRe.MatchString(name) {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".":
			continue
		case "..":
			return ""
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return ""
	}
	return filepath.Join(append([]string{dest}, parts...)...)
}

// steamMemberMode: Valve's zips store 0666 for everything. Until Steam sets its own modes, executables (no
// extension, or a .sh script) get 0755 and the rest 0644, as boppos-tools does.
func steamMemberMode(name string) os.FileMode {
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if strings.HasSuffix(base, ".sh") || !strings.Contains(base, ".") {
		return 0o755
	}
	return 0o644
}

func isSteamUI(name string) bool {
	return filepath.Base(strings.ReplaceAll(name, `\`, "/")) == "steamui.so"
}

// writeSteamMember writes one zip member to target through a temporary file in the same folder.
func writeSteamMember(zf *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && (info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("%s: something else is in the way", zf.Name)
	}
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("%s: %w", zf.Name, err)
	}
	defer rc.Close()
	tmp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".boppos-tmp")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, steamMemberMode(zf.Name))
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, steamMemberMode(zf.Name))
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s: %w", zf.Name, err)
	}
	return nil
}

// unpackSteamClient unpacks every package of the manifest from pkgDir into steamDir. steamui.so is written only
// after every other file of every package is in place. It returns how many files it wrote.
func unpackSteamClient(steamDir, pkgDir string, m *SteamManifest) (int, error) {
	scratch, err := os.MkdirTemp(steamDir, ".boppos-unpack-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(scratch)

	type later struct {
		zipPath string
		name    string
	}
	var last []later
	files := 0
	for _, p := range m.Packages {
		zipPath := filepath.Join(pkgDir, p.Name)
		if strings.Contains(p.Name, ".zip.vz.") {
			zipPath = filepath.Join(scratch, p.Name+".zip")
			if err := unvz(filepath.Join(pkgDir, p.Name), zipPath); err != nil {
				return files, err
			}
		}
		zr, err := zip.OpenReader(zipPath)
		if err != nil {
			return files, fmt.Errorf("%s: %w", p.Name, err)
		}
		for _, zf := range zr.File {
			target := steamMemberPath(steamDir, zf.Name)
			if target == "" {
				zr.Close()
				return files, fmt.Errorf("%s: unsafe path %q", p.Name, zf.Name)
			}
			switch {
			case strings.HasSuffix(zf.Name, "/") || strings.HasSuffix(zf.Name, `\`) || zf.FileInfo().IsDir():
				err = os.MkdirAll(target, 0o755)
			case isSteamUI(zf.Name):
				last = append(last, later{zipPath, zf.Name})
			default:
				err = writeSteamMember(zf, target)
				files++
			}
			if err != nil {
				zr.Close()
				return files, fmt.Errorf("%s: %w", p.Name, err)
			}
		}
		zr.Close()
	}
	if len(last) == 0 {
		return files, errors.New("the client packages hold no steamui.so")
	}
	for _, l := range last {
		zr, err := zip.OpenReader(l.zipPath)
		if err != nil {
			return files, err
		}
		for _, zf := range zr.File {
			if zf.Name == l.name {
				err = writeSteamMember(zf, steamMemberPath(steamDir, zf.Name))
				files++
				break
			}
		}
		zr.Close()
		if err != nil {
			return files, err
		}
	}
	return files, nil
}

// steamStartsOnDeckBranch reports whether the image starts Steam with -steamdeck: CachyOS-Handheld patches the
// flag into /usr/bin/steam, which Game Mode and the desktop both run.
func steamStartsOnDeckBranch(deployDir string) bool {
	data, err := os.ReadFile(filepath.Join(deployDir, "usr", "bin", "steam"))
	return err == nil && bytes.Contains(data, []byte("-steamdeck"))
}

// steamLinks makes ~/.steam/root and ~/.steam/steam point at the Steam folder, as steam.sh does on its first
// start. passwdHome is the home as the installed system sees it (from etc/passwd), so the links resolve there.
// Existing entries are left alone. Everything created goes to uid:gid.
func steamLinks(home, passwdHome string, uid, gid int) error {
	dotSteam := filepath.Join(home, ".steam")
	if _, err := os.Lstat(dotSteam); err != nil {
		if err := os.Mkdir(dotSteam, 0o755); err != nil {
			return err
		}
		if err := chownFn(dotSteam, uid, gid); err != nil {
			return err
		}
	}
	target := filepath.Join(passwdHome, ".local", "share", "Steam")
	for _, name := range []string{"root", "steam"} {
		link := filepath.Join(dotSteam, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		if err := chownFn(link, uid, gid); err != nil {
			return err
		}
	}
	return nil
}
