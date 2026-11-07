package post

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tuna-os/fisherman/internal/runner"
)

// NMConnectionsDir is where the live session's NetworkManager keeps the profiles the user made (nmtui, nmcli or the
// installer's network page). Tests point it at a temporary directory.
var NMConnectionsDir = "/etc/NetworkManager/system-connections"

// chownFn changes a file's owner. fisherman runs as root, so this is os.Lchown; tests replace it, because an
// unprivileged test cannot hand a file to root.
var chownFn = os.Lchown

// carriedConnectionTypes are the NetworkManager profile types the installed system gets: Wi-Fi and wired. VPNs,
// bridges, hotspots and the like are left behind; they belong to the live session, not the machine.
var carriedConnectionTypes = map[string]bool{
	"wifi":            true,
	"802-11-wireless": true,
	"ethernet":        true,
	"802-3-ethernet":  true,
}

// nmProfile is what CopyWiFiConnections reads from a keyfile's [connection] section.
type nmProfile struct {
	id          string
	connType    string
	permissions string
}

// parseNMProfile reads the [connection] section of a NetworkManager keyfile. It never keeps secrets: only the id,
// type and permissions keys are read.
func parseNMProfile(data []byte) nmProfile {
	var p nmProfile
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "connection" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "id":
			p.id = strings.TrimSpace(value)
		case "type":
			p.connType = strings.TrimSpace(value)
		case "permissions":
			p.permissions = strings.TrimSpace(value)
		}
	}
	return p
}

// CopyWiFiConnections copies the Wi-Fi and wired NetworkManager profiles the user set up in the live session into the
// installed system, so the first boot is online without asking again (Steam's first start needs the network).
//
// On ostree the profiles go into the new deployment's etc (ostree/deploy/<os>/deploy/<checksum>.0/etc), which is the
// booted system's /etc; on composefs-native into the deploy etc. Each file is written 0600 root:root, the only mode
// NetworkManager loads a keyfile with. Skipped: other profile types (VPN, bridge, ...), and profiles limited to a
// user ("permissions=user:liveuser"), whose secrets live in that user's keyring and whose user the installed system
// does not have. The log names the profiles it copies or skips, never their contents.
//
// Non-fatal: the caller turns an error into a warning, and the system still boots without the profiles.
func CopyWiFiConnections(target string) error {
	entries, err := os.ReadDir(NMConnectionsDir)
	if err != nil {
		fmt.Fprintf(os.Stdout, "  no NetworkManager profiles to carry over (%s: %v)\n", NMConnectionsDir, err)
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	type carried struct {
		name, id string
		data     []byte
	}
	var keep []carried
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".nmconnection") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(NMConnectionsDir, name))
		if err != nil {
			fmt.Fprintf(os.Stdout, "  skipped NetworkManager profile %s: %v\n", name, err)
			continue
		}
		p := parseNMProfile(data)
		label := p.id
		if label == "" {
			label = strings.TrimSuffix(name, ".nmconnection")
		}
		switch {
		case !carriedConnectionTypes[p.connType]:
			fmt.Fprintf(os.Stdout, "  skipped NetworkManager profile %q: type %q is not Wi-Fi or ethernet\n", label, p.connType)
		case p.permissions != "":
			fmt.Fprintf(os.Stdout, "  skipped NetworkManager profile %q: limited to a live-session user\n", label)
		default:
			keep = append(keep, carried{name, label, data})
		}
	}
	if len(keep) == 0 {
		fmt.Fprintf(os.Stdout, "  no Wi-Fi or ethernet profiles in %s to carry over\n", NMConnectionsDir)
		return nil
	}

	etcDir, err := deployEtcDir(target)
	if err != nil {
		return fmt.Errorf("finding the installed system's etc for NetworkManager profiles: %w", err)
	}
	dst := filepath.Join(etcDir, "NetworkManager", "system-connections")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	if err := os.Chmod(dst, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}

	var copied []string
	for _, c := range keep {
		path := filepath.Join(dst, c.name)
		if err := writeRootSecret(path, c.data); err != nil {
			return fmt.Errorf("writing NetworkManager profile %q: %w", c.id, err)
		}
		copied = append(copied, fmt.Sprintf("%q", c.id))
	}
	// SELinux images: label the files for NetworkManager. A no-op where restorecon is missing.
	_ = runner.Run("restorecon", "-R", dst)
	fmt.Fprintf(os.Stdout, "  carried over NetworkManager profiles %s to %s\n", strings.Join(copied, ", "), dst)
	return nil
}

// writeRootSecret writes data to path as a 0600 root:root file, through a temporary file so a half-written profile is
// never left behind. The mode is set before any data is written.
func writeRootSecret(path string, data []byte) error {
	tmp := path + ".fisherman-tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := chownFn(tmp, 0, 0); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
