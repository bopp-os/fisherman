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
	uuid        string
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
		case "uuid":
			p.uuid = strings.TrimSpace(value)
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
		name, id, uuid string
		data           []byte
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
			portable, notes := portableNMProfile(data)
			for _, n := range notes {
				fmt.Fprintf(os.Stdout, "  NetworkManager profile %q: %s\n", label, n)
			}
			keep = append(keep, carried{name, label, p.uuid, portable})
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

	// Profiles already in the installed system (kept from the old install by a keep-files install) win over the live
	// session's: same file name, same uuid or same id.
	have := map[string]bool{}
	if existing, err := os.ReadDir(dst); err == nil {
		for _, e := range existing {
			have["file:"+e.Name()] = true
			if data, err := os.ReadFile(filepath.Join(dst, e.Name())); err == nil {
				ep := parseNMProfile(data)
				if ep.uuid != "" {
					have["uuid:"+ep.uuid] = true
				}
				if ep.id != "" {
					have["id:"+ep.id] = true
				}
			}
		}
	}

	var copied []string
	for _, c := range keep {
		if have["file:"+c.name] || (c.uuid != "" && have["uuid:"+c.uuid]) || have["id:"+c.id] {
			fmt.Fprintf(os.Stdout, "  NetworkManager profile %q: the installed system already has it, kept that one\n", c.id)
			continue
		}
		path := filepath.Join(dst, c.name)
		if err := writeRootSecret(path, c.data); err != nil {
			return fmt.Errorf("writing NetworkManager profile %q: %w", c.id, err)
		}
		copied = append(copied, fmt.Sprintf("%q", c.id))
	}
	// SELinux images: label the files for NetworkManager. A no-op where restorecon is missing.
	_ = runner.Run("restorecon", "-R", dst)
	if len(copied) == 0 {
		return nil
	}
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

// portableNMProfile returns a live-session keyfile as the installed system should get it, with a note for each
// change (never a secret, MAC address or password in a note):
//   - interface-name and the wifi/ethernet mac-address binding are dropped: the installed system may name or
//     randomize the device differently (iwd renames Wi-Fi devices to wlan0), and a profile bound to another name
//     never activates;
//   - autoconnect=false is dropped, so the profile connects on boot;
//   - a WPA/SAE profile without a saved password, or whose password lives in a secret agent (psk-flags other than
//     0), gets a note: it cannot connect on its own.
func portableNMProfile(data []byte) ([]byte, []string) {
	var out bytes.Buffer
	var notes []string
	section, keyMgmt, psk, pskFlags := "", "", "", ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
		} else if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			drop := ""
			switch section {
			case "connection":
				if key == "interface-name" && value != "" {
					drop = fmt.Sprintf("dropped interface-name=%s (the installed system may name the device differently)", value)
				}
				if key == "autoconnect" && value == "false" {
					drop = "dropped autoconnect=false, so it connects on boot"
				}
			case "wifi", "802-11-wireless", "ethernet", "802-3-ethernet":
				if key == "mac-address" && value != "" {
					drop = "dropped its binding to the live session's device MAC address"
				}
			case "wifi-security", "802-11-wireless-security":
				switch key {
				case "key-mgmt":
					keyMgmt = value
				case "psk":
					psk = value
				case "psk-flags":
					pskFlags = value
				}
			}
			if drop != "" {
				notes = append(notes, drop)
				continue
			}
		}
		out.WriteString(raw)
		out.WriteByte('\n')
	}
	if keyMgmt == "wpa-psk" || keyMgmt == "sae" {
		if psk == "" || (pskFlags != "" && pskFlags != "0") {
			notes = append(notes, fmt.Sprintf("WARNING: no saved Wi-Fi password in the profile (psk-flags=%q); "+
				"it cannot connect on its own", pskFlags))
		} else {
			notes = append(notes, "Wi-Fi password saved in the profile")
		}
	}
	return out.Bytes(), notes
}
