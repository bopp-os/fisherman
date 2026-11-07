package post

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/fisherman/internal/runner"
)

// Steam client preload.
//
// The first start of Steam on a fresh account extracts the bootstrap that the steam package ships
// (/usr/lib/steam/bootstraplinux_ubuntu12_32.tar.xz) into ~/.local/share/Steam, then downloads the client: Valve's
// manifest https://client-update.steamstatic.com/steam_client_ubuntu12 names about 30 packages (~430 MB compressed).
// In Game Mode that download runs behind a black screen for minutes. PreloadSteam does the same steps at install
// time, while the live system is online: it extracts the bootstrap and stores the manifest and every package in
// ~/.local/share/Steam/package/ under the names Steam itself uses (the ".zip.vz.<sha>_<size>" form when the manifest
// has one, else the plain ".zip.<sha>"). On its first start Steam finds the packages in place with the right sizes and
// only extracts them; when Valve has released a newer client in between, it downloads just the packages that changed.
// No ".installed" marker is written: extracting the packages stays Steam's job.

// SteamCDN is Valve's client update host. Tests point it at a local server.
var SteamCDN = "https://client-update.steamstatic.com"

// steamHTTPClient downloads the manifest and the packages. Timeouts are per request; PreloadSteam also has an overall
// deadline (SteamPreloadTimeout).
var steamHTTPClient = &http.Client{Timeout: 10 * time.Minute}

// SteamPreloadTimeout caps the whole preload so a slow network delays the install by at most this long. Packages
// downloaded before the deadline are kept; Steam fetches the rest on its first start.
var SteamPreloadTimeout = 20 * time.Minute

// steamManifestName is the Linux client manifest (the stable branch, which is also what the Deck's steamdeck_stable
// branch serves today) and the file name Steam stores it under in package/.
const (
	steamManifestName = "steam_client_ubuntu12"
	steamBootstrapRel = "usr/lib/steam/bootstraplinux_ubuntu12_32.tar.xz"
)

// steamPackageNameRe is what a package file name from the manifest must look like before it becomes a path or a URL.
var steamPackageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// SteamPackage is one client package to download: the file name Steam keeps in package/, its size and SHA-256.
type SteamPackage struct {
	Name   string
	Size   int64
	SHA256 string
}

// SteamManifest is the part of the client manifest PreloadSteam uses.
type SteamManifest struct {
	Version  string
	Packages []SteamPackage
}

// ParseSteamManifest reads Valve's client manifest (Valve KeyValues text) and returns its version and the package
// files to download. For each package the compressed form (zipvz, sha2vz, size from the name's "_<size>" suffix) is
// preferred, as Steam does; packages with only a plain zip use file, size and sha2.
func ParseSteamManifest(data []byte) (*SteamManifest, error) {
	root, err := parseKeyValues(data)
	if err != nil {
		return nil, err
	}
	var body kvNode
	for _, n := range root.children {
		if n.key == "ubuntu12" && n.children != nil {
			body = n
			break
		}
	}
	if body.children == nil {
		return nil, errors.New("manifest has no \"ubuntu12\" section")
	}
	m := &SteamManifest{}
	for _, n := range body.children {
		if n.children == nil {
			if n.key == "version" {
				m.Version = n.value
			}
			continue
		}
		fields := map[string]string{}
		for _, f := range n.children {
			if f.children == nil {
				fields[f.key] = f.value
			}
		}
		var p SteamPackage
		if vz := fields["zipvz"]; vz != "" && fields["sha2vz"] != "" {
			i := strings.LastIndex(vz, "_")
			if i < 0 {
				return nil, fmt.Errorf("package %s: zipvz name %q has no size suffix", n.key, vz)
			}
			size, err := strconv.ParseInt(vz[i+1:], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("package %s: zipvz size in %q: %w", n.key, vz, err)
			}
			p = SteamPackage{Name: vz, Size: size, SHA256: fields["sha2vz"]}
		} else if fields["file"] != "" {
			size, err := strconv.ParseInt(fields["size"], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("package %s: size %q: %w", n.key, fields["size"], err)
			}
			p = SteamPackage{Name: fields["file"], Size: size, SHA256: fields["sha2"]}
		} else {
			continue
		}
		if !steamPackageNameRe.MatchString(p.Name) || strings.Contains(p.Name, "..") {
			return nil, fmt.Errorf("package %s: unsafe file name %q", n.key, p.Name)
		}
		if len(p.SHA256) != 64 {
			return nil, fmt.Errorf("package %s: no SHA-256", n.key)
		}
		if p.Size <= 0 {
			return nil, fmt.Errorf("package %s: size %d", n.key, p.Size)
		}
		m.Packages = append(m.Packages, p)
	}
	if m.Version == "" {
		return nil, errors.New("manifest has no version")
	}
	if len(m.Packages) == 0 {
		return nil, errors.New("manifest lists no packages")
	}
	return m, nil
}

// kvNode is one Valve KeyValues entry: a key with either a string value or child entries.
type kvNode struct {
	key      string
	value    string
	children []kvNode
}

// parseKeyValues parses Valve's KeyValues text format: quoted (or bare) keys followed by a quoted value or a braced
// block, with // comments. It is just enough for the client manifest.
func parseKeyValues(data []byte) (kvNode, error) {
	toks, err := kvTokens(string(data))
	if err != nil {
		return kvNode{}, err
	}
	pos := 0
	var parse func(depth int) ([]kvNode, error)
	parse = func(depth int) ([]kvNode, error) {
		nodes := []kvNode{}
		for pos < len(toks) {
			t := toks[pos]
			if t.kind == '}' {
				if depth == 0 {
					return nil, errors.New("unexpected } in manifest")
				}
				pos++
				return nodes, nil
			}
			if t.kind != 's' {
				return nil, fmt.Errorf("unexpected %q in manifest", string(t.kind))
			}
			pos++
			if pos >= len(toks) {
				return nil, fmt.Errorf("key %q has no value", t.text)
			}
			switch v := toks[pos]; v.kind {
			case 's':
				pos++
				nodes = append(nodes, kvNode{key: t.text, value: v.text})
			case '{':
				pos++
				kids, err := parse(depth + 1)
				if err != nil {
					return nil, err
				}
				nodes = append(nodes, kvNode{key: t.text, children: kids})
			default:
				return nil, fmt.Errorf("key %q has no value", t.text)
			}
		}
		if depth != 0 {
			return nil, errors.New("manifest ends inside a block")
		}
		return nodes, nil
	}
	kids, err := parse(0)
	if err != nil {
		return kvNode{}, err
	}
	return kvNode{children: kids}, nil
}

type kvToken struct {
	kind rune // 's' string, '{', '}'
	text string
}

func kvTokens(s string) ([]kvToken, error) {
	var toks []kvToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '{' || c == '}':
			toks = append(toks, kvToken{kind: rune(c)})
			i++
		case c == '"':
			var b strings.Builder
			i++
			for {
				if i >= len(s) {
					return nil, errors.New("unterminated string in manifest")
				}
				if s[i] == '\\' && i+1 < len(s) {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			toks = append(toks, kvToken{kind: 's', text: b.String()})
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\r\n{}\"", rune(s[j])) {
				j++
			}
			toks = append(toks, kvToken{kind: 's', text: s[i:j]})
			i = j
		}
	}
	return toks, nil
}

// lookupPasswd returns the UID and GID of username from a passwd file.
func lookupPasswd(passwdPath, username string) (int, int, error) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 || fields[0] != username {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			return 0, 0, fmt.Errorf("uid of %s: %w", username, err)
		}
		gid, err := strconv.Atoi(fields[3])
		if err != nil {
			return 0, 0, fmt.Errorf("gid of %s: %w", username, err)
		}
		return uid, gid, nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf("%s is not in %s", username, passwdPath)
}

// PreloadSteam fills ~/.local/share/Steam of the account fisherman just created with the Steam bootstrap and the
// current client packages, so the first Game Mode start does not download the client (see the comment at the top of
// this file). It does nothing when the image ships no Steam bootstrap or the account is missing.
//
// ostree only: the home is the stateroot's var/home/<user> (where CreateUser put it), the UID and GID come from the
// deployment's etc/passwd, and every file it writes is handed to that user. composefs-native homes are created from
// /etc/skel on the first boot, which a preloaded home would block, so those are skipped.
//
// Never fatal: the caller turns an error into a warning. On any failure before the first package is stored the Steam
// folder is removed again, so the first start behaves exactly as without a preload.
func PreloadSteam(target, username string) error {
	if username == "" {
		return nil
	}
	if isComposeFsNative(target) {
		fmt.Fprintln(os.Stdout, "  Steam preload skipped: not supported on composefs-native installs")
		return nil
	}
	deployDir, err := DeploymentDirFn(target)
	if err != nil {
		return fmt.Errorf("finding deployment dir: %w", err)
	}
	bootstrap := filepath.Join(deployDir, steamBootstrapRel)
	if _, err := os.Stat(bootstrap); err != nil {
		fmt.Fprintf(os.Stdout, "  Steam preload skipped: the image has no %s\n", "/"+steamBootstrapRel)
		return nil
	}
	uid, gid, err := lookupPasswd(filepath.Join(deployDir, "etc", "passwd"), username)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", username, err)
	}
	home := filepath.Join(target, "ostree", "deploy", "default", "var", "home", username)
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		return fmt.Errorf("home of %s not found at %s", username, home)
	}
	steamDir := filepath.Join(home, ".local", "share", "Steam")
	if _, err := os.Lstat(steamDir); err == nil {
		fmt.Fprintf(os.Stdout, "  Steam preload skipped: %s already exists\n", steamDir)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), SteamPreloadTimeout)
	defer cancel()

	// The manifest first: offline, nothing is written to the home at all.
	manifestData, err := steamGet(ctx, steamManifestName, 1<<20)
	if err != nil {
		return fmt.Errorf("downloading the Steam client manifest (offline?): %w", err)
	}
	manifest, err := ParseSteamManifest(manifestData)
	if err != nil {
		return fmt.Errorf("reading the Steam client manifest: %w", err)
	}

	// ~/.local and ~/.local/share may not exist yet; each directory created here goes to the user.
	for _, dir := range []string{filepath.Join(home, ".local"), filepath.Join(home, ".local", "share")} {
		if _, err := os.Lstat(dir); err == nil {
			continue
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		if err := chownFn(dir, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", dir, err)
		}
	}
	// Steam's launcher creates its folder with umask 0077; do the same.
	if err := os.Mkdir(steamDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", steamDir, err)
	}
	stored := 0
	defer func() {
		if stored == 0 {
			_ = os.RemoveAll(steamDir)
		}
	}()
	if err := runner.Run("tar", "-xJf", bootstrap, "-C", steamDir, "--no-same-owner"); err != nil {
		return fmt.Errorf("extracting the Steam bootstrap: %w", err)
	}
	pkgDir := filepath.Join(steamDir, "package")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", pkgDir, err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, steamManifestName+".manifest"), manifestData, 0o600); err != nil {
		return fmt.Errorf("writing the Steam client manifest: %w", err)
	}

	var total int64
	for _, p := range manifest.Packages {
		total += p.Size
	}
	fmt.Fprintf(os.Stdout, "  preloading Steam client %s for %s: %d packages, %s\n",
		manifest.Version, username, len(manifest.Packages), humanBytes(total))

	var got int64
	var failed []string
	var stopErr error
	for i, p := range manifest.Packages {
		if stopErr != nil {
			failed = append(failed, p.Name)
			continue
		}
		if err := steamDownloadPackage(ctx, pkgDir, p); err != nil {
			failed = append(failed, p.Name)
			fmt.Fprintf(os.Stdout, "  Steam package %s not preloaded: %v\n", p.Name, err)
			// A dead network or the deadline ends the preload; a bad file only skips that package.
			if ctx.Err() != nil || !errors.Is(err, errSteamChecksum) {
				stopErr = err
			}
			continue
		}
		stored++
		got += p.Size
		if (i+1)%5 == 0 || i+1 == len(manifest.Packages) {
			fmt.Fprintf(os.Stdout, "  Steam client: %d/%d packages, %s of %s\n",
				i+1, len(manifest.Packages), humanBytes(got), humanBytes(total))
		}
	}
	if stored == 0 {
		return fmt.Errorf("no Steam package could be downloaded: %w", stopErr)
	}

	// Everything under the Steam folder goes to the user (tar ran as root).
	err = filepath.WalkDir(steamDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return chownFn(path, uid, gid)
	})
	if err != nil {
		// The first boot's tmpfiles "Z /var/home/<user>" (writeHomeTmpfiles) fixes ownership too, so keep the files.
		return fmt.Errorf("handing the Steam folder to %s: %w", username, err)
	}

	if len(failed) > 0 {
		return fmt.Errorf("Steam preloaded partly (%d of %d packages, the rest downloads on first start): %w",
			stored, len(manifest.Packages), stopErr)
	}
	fmt.Fprintf(os.Stdout, "  preloaded the Steam client (%s) into %s\n", humanBytes(got), steamDir)
	return nil
}

var errSteamChecksum = errors.New("checksum mismatch")

// steamGet downloads a small file from the Steam CDN into memory, refusing more than limit bytes.
func steamGet(ctx context.Context, name string, limit int64) ([]byte, error) {
	resp, err := steamRequest(ctx, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, nil
}

func steamRequest(ctx context.Context, name string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(SteamCDN, "/")+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := steamHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, name)
	}
	return resp, nil
}

// steamDownloadPackage downloads one package into pkgDir, checking its size and SHA-256 before it takes its final
// name. A failed download leaves nothing behind.
func steamDownloadPackage(ctx context.Context, pkgDir string, p SteamPackage) error {
	resp, err := steamRequest(ctx, p.Name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	final := filepath.Join(pkgDir, p.Name)
	tmp := final + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, p.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != p.Size {
		err = fmt.Errorf("%w: got %d bytes, want %d", errSteamChecksum, n, p.Size)
	}
	if err == nil && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), p.SHA256) {
		err = fmt.Errorf("%w: SHA-256 differs from the manifest", errSteamChecksum)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, final)
}
