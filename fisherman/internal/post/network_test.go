package post

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeOstreeTarget makes <target>/ostree/deploy/default/deploy/<csum>.0/etc and points DeploymentDirFn at it.
func fakeOstreeTarget(t *testing.T) (target, deployDir string) {
	t.Helper()
	target = t.TempDir()
	deployDir = filepath.Join(target, "ostree", "deploy", "default", "deploy", "abc123.0")
	if err := os.MkdirAll(filepath.Join(deployDir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := DeploymentDirFn
	DeploymentDirFn = func(string) (string, error) { return deployDir, nil }
	t.Cleanup(func() { DeploymentDirFn = old })
	return target, deployDir
}

type chownCall struct {
	path     string
	uid, gid int
}

// recordChown replaces chownFn with a recorder.
func recordChown(t *testing.T) *[]chownCall {
	t.Helper()
	var calls []chownCall
	old := chownFn
	chownFn = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return nil
	}
	t.Cleanup(func() { chownFn = old })
	return &calls
}

func liveConnections(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := NMConnectionsDir
	NMConnectionsDir = dir
	t.Cleanup(func() { NMConnectionsDir = old })
}

const wifiProfile = `[connection]
id=Home Wi-Fi
uuid=0b8f6a1e-8f1c-4c55-9f83-2b4c6d0f7a11
type=wifi

[wifi]
mode=infrastructure
ssid=Home Wi-Fi

[wifi-security]
key-mgmt=wpa-psk
psk=hunter2-secret
`

func TestCopyWiFiConnectionsCarriesWifiAndEthernetIntoTheDeploymentEtc(t *testing.T) {
	target, deployDir := fakeOstreeTarget(t)
	chowns := recordChown(t)
	liveConnections(t, map[string]string{
		"Home Wi-Fi.nmconnection": wifiProfile,
		"Wired.nmconnection":      "[connection]\nid=Wired\ntype=ethernet\n",
		"Old.nmconnection":        "[connection]\nid=Old\ntype=802-11-wireless\n",
	})

	out := captureStdout(t, func() {
		if err := CopyWiFiConnections(target); err != nil {
			t.Fatal(err)
		}
	})

	dst := filepath.Join(deployDir, "etc", "NetworkManager", "system-connections")
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("system-connections dir: %v %v", info, err)
	}
	got, _ := os.ReadDir(dst)
	var names []string
	for _, e := range got {
		names = append(names, e.Name())
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}
	sort.Strings(names)
	want := []string{"Home Wi-Fi.nmconnection", "Old.nmconnection", "Wired.nmconnection"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("copied %v, want %v", names, want)
	}
	body, _ := os.ReadFile(filepath.Join(dst, "Home Wi-Fi.nmconnection"))
	if string(body) != wifiProfile {
		t.Errorf("profile content changed:\n%s", body)
	}
	if len(*chowns) != 3 {
		t.Errorf("chown calls %v, want one per profile", *chowns)
	}
	for _, c := range *chowns {
		if c.uid != 0 || c.gid != 0 {
			t.Errorf("%s handed to %d:%d, want root", c.path, c.uid, c.gid)
		}
	}
	if !strings.Contains(out, `"Home Wi-Fi"`) {
		t.Errorf("log does not name the profile: %q", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("log leaks the Wi-Fi password: %q", out)
	}
	// Never the sysroot's own etc.
	if _, err := os.Stat(filepath.Join(target, "etc", "NetworkManager")); err == nil {
		t.Error("profiles written to <sysroot>/etc")
	}
}

func TestCopyWiFiConnectionsLeavesOtherProfilesBehind(t *testing.T) {
	target, deployDir := fakeOstreeTarget(t)
	recordChown(t)
	liveConnections(t, map[string]string{
		"vpn.nmconnection":       "[connection]\nid=Work VPN\ntype=vpn\n",
		"mine.nmconnection":      "[connection]\nid=Phone\ntype=wifi\npermissions=user:liveuser;\n",
		"notes.txt":              "not a profile",
		"bridge.nmconnection":    "[connection]\nid=br0\ntype=bridge\n",
		"nosection.nmconnection": "id=x\ntype=wifi\n",
	})

	out := captureStdout(t, func() {
		if err := CopyWiFiConnections(target); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(deployDir, "etc", "NetworkManager")); err == nil {
		t.Error("created NetworkManager config with nothing to copy")
	}
	for _, want := range []string{`"Work VPN"`, `"Phone": limited to a live-session user`, "no Wi-Fi or ethernet profiles"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
}

func TestCopyWiFiConnectionsWithoutLiveProfilesIsANoOp(t *testing.T) {
	target, _ := fakeOstreeTarget(t)
	old := NMConnectionsDir
	NMConnectionsDir = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { NMConnectionsDir = old })
	if err := CopyWiFiConnections(target); err != nil {
		t.Fatal(err)
	}
}

func TestParseNMProfileReadsOnlyTheConnectionSection(t *testing.T) {
	p := parseNMProfile([]byte("# comment\n[connection]\nid = Cafe \ntype=wifi\n[wifi]\nid=ignored\ntype=ignored\n"))
	if p.id != "Cafe" || p.connType != "wifi" || p.permissions != "" {
		t.Fatalf("got %+v", p)
	}
}
