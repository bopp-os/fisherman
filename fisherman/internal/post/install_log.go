package post

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// InstallLogSource is what fisherman's frontends redirect its output to (`fisherman - >log 2>&1`): /proc/self/fd/1
// opens that file afresh from the start. Tests point it at a temporary file.
var InstallLogSource = "/proc/self/fd/1"

// InstallLogRel is where the installed system keeps the log, relative to its /var.
const InstallLogRel = "log/fisherman/install.log"

// SaveInstallLog copies everything fisherman has logged so far into the installed system's
// /var/log/fisherman/install.log (0600 root:root), so an install can be looked at after the reboot, when the live
// session and its log are gone. Lines carrying the LUKS recovery key are left out.
//
// Nothing happens when fisherman's output is not a regular file (a terminal or a pipe). Non-fatal: the caller turns
// an error into a warning. Call it last before the target is finalized; later lines are not in the copy.
func SaveInstallLog(target string) error {
	info, err := os.Stat(InstallLogSource)
	if err != nil || !info.Mode().IsRegular() {
		return nil //nolint:nilerr // output is not a file: nothing to save
	}
	data, err := os.ReadFile(InstallLogSource)
	if err != nil {
		return fmt.Errorf("reading the install log: %w", err)
	}

	varDir := filepath.Join(target, "ostree", "deploy", "default", "var")
	if isComposeFsNative(target) {
		varDir = ComposeFsVarDir(target)
	}
	path := filepath.Join(varDir, InstallLogRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := writeRootSecret(path, redactInstallLog(data)); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stdout, "  saved the install log as /var/%s\n", InstallLogRel)
	return nil
}

// redactInstallLog drops the progress events that carry the LUKS recovery key.
func redactInstallLog(data []byte) []byte {
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"recovery_key"`)) {
			out.WriteString("[recovery key left out]\n")
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
