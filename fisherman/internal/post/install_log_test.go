package post

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installLogSource(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fisherman-output.log")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := InstallLogSource
	InstallLogSource = path
	t.Cleanup(func() { InstallLogSource = old })
}

func TestSaveInstallLogCopiesTheLogIntoTheStaterootVar(t *testing.T) {
	target, _ := fakeOstreeTarget(t)
	chowns := recordChown(t)
	installLogSource(t, "+ bootc install\n{\"type\":\"recovery_key\",\"key\":\"ABCD-EFGH\"}\n{\"type\":\"info\",\"message\":\"done\"}\n")

	captureStdout(t, func() {
		if err := SaveInstallLog(target); err != nil {
			t.Fatal(err)
		}
	})

	path := filepath.Join(target, "ostree", "deploy", "default", "var", "log", "fisherman", "install.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "ABCD-EFGH") {
		t.Error("the recovery key is in the saved log")
	}
	for _, want := range []string{"+ bootc install", `"message":"done"`, "[recovery key left out]"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("saved log lacks %q:\n%s", want, got)
		}
	}
	if len(*chowns) != 1 || (*chowns)[0].uid != 0 || (*chowns)[0].gid != 0 {
		t.Errorf("chown calls %v, want the log handed to root", *chowns)
	}
}

func TestSaveInstallLogWithoutALogFileIsANoOp(t *testing.T) {
	target, _ := fakeOstreeTarget(t)
	recordChown(t)
	old := InstallLogSource
	InstallLogSource = t.TempDir() // a directory, like a terminal or pipe: not a regular file
	t.Cleanup(func() { InstallLogSource = old })

	if err := SaveInstallLog(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "ostree", "deploy", "default", "var", "log")); err == nil {
		t.Error("created /var/log without a log to save")
	}
}
