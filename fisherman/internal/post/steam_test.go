package post

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn and returns what it printed to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// steamFixture is a fake Valve CDN: a manifest with one compressed package, one plain-zip package and their bodies.
type steamFixture struct {
	manifest []byte
	files    map[string][]byte
	hits     map[string]int
}

func newSteamFixture() *steamFixture {
	vz := []byte("compressed bins package")
	zip := []byte("plain zip package body")
	vzName := fmt.Sprintf("bins_ubuntu12.zip.vz.1111111111111111111111111111111111111111_%d", len(vz))
	zipName := "public_all.zip.2222222222222222222222222222222222222222"
	manifest := fmt.Sprintf(`"ubuntu12"
{
	"version"		"1792618285"
	// comment line
	"bins_ubuntu12"
	{
		"file"		"bins_ubuntu12.zip.aaaa"
		"size"		"999999"
		"sha2"		"%s"
		"zipvz"		"%s"
		"sha2vz"		"%s"
	}
	"public_all"
	{
		"file"		"%s"
		"size"		"%d"
		"sha2"		"%s"
		"IsBootstrapperPackage"		"1"
	}
}
"kvsignatures"
{
	"ubuntu12"		"deadbeef"
}
`, strings.Repeat("0", 64), vzName, sha(vz), zipName, len(zip), sha(zip))
	return &steamFixture{
		manifest: []byte(manifest),
		files:    map[string][]byte{vzName: vz, zipName: zip},
		hits:     map[string]int{},
	}
}

func (f *steamFixture) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		f.hits[name]++
		if name == steamManifestName {
			w.Write(f.manifest)
			return
		}
		if b, ok := f.files[name]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	old := SteamCDN
	SteamCDN = srv.URL
	t.Cleanup(func() { SteamCDN = old })
}

// steamTarget makes an ostree target whose image ships the Steam bootstrap, with user gamer (1000:1000) and its home.
func steamTarget(t *testing.T, withBootstrap bool) (target, home string) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	target, deployDir := fakeOstreeTarget(t)
	if err := os.WriteFile(filepath.Join(deployDir, "etc", "passwd"),
		[]byte("root:x:0:0::/root:/bin/bash\ngamer:x:1000:1001:Gamer:/home/gamer:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(target, "ostree", "deploy", "default", "var", "home", "gamer")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if withBootstrap {
		src := t.TempDir()
		os.MkdirAll(filepath.Join(src, "ubuntu12_32"), 0o755)
		os.WriteFile(filepath.Join(src, "steam.sh"), []byte("#!/bin/sh\n"), 0o755)
		os.WriteFile(filepath.Join(src, "ubuntu12_32", "steam"), []byte("bootstrapper"), 0o755)
		dir := filepath.Join(deployDir, "usr", "lib", "steam")
		os.MkdirAll(dir, 0o755)
		out, err := exec.Command("tar", "-cJf", filepath.Join(dir, "bootstraplinux_ubuntu12_32.tar.xz"),
			"-C", src, "steam.sh", "ubuntu12_32").CombinedOutput()
		if err != nil {
			t.Skipf("cannot build an xz tarball here: %v %s", err, out)
		}
	}
	return target, home
}

func TestParseSteamManifestPrefersTheCompressedPackage(t *testing.T) {
	f := newSteamFixture()
	m, err := ParseSteamManifest(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1792618285" || len(m.Packages) != 2 {
		t.Fatalf("got %+v", m)
	}
	vz := m.Packages[0]
	if !strings.HasPrefix(vz.Name, "bins_ubuntu12.zip.vz.") || vz.Size != int64(len("compressed bins package")) {
		t.Errorf("compressed package: %+v", vz)
	}
	if m.Packages[1].Name != "public_all.zip.2222222222222222222222222222222222222222" {
		t.Errorf("plain package: %+v", m.Packages[1])
	}
}

func TestParseSteamManifestRefusesUnsafeNames(t *testing.T) {
	for _, name := range []string{"../evil.zip", "dir/evil.zip", "..zip"} {
		m := fmt.Sprintf(`"ubuntu12" { "version" "1" "p" { "file" "%s" "size" "3" "sha2" "%s" } }`, name, strings.Repeat("a", 64))
		if _, err := ParseSteamManifest([]byte(m)); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	for _, bad := range []string{``, `"ubuntu12" {`, `"other" { "version" "1" }`, `"ubuntu12" { "version" "1" }`} {
		if _, err := ParseSteamManifest([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPreloadSteamStoresTheClientPackagesForTheNewUser(t *testing.T) {
	target, home := steamTarget(t, true)
	chowns := recordChown(t)
	f := newSteamFixture()
	f.serve(t)

	captureStdout(t, func() {
		if err := PreloadSteam(target, "gamer"); err != nil {
			t.Fatal(err)
		}
	})

	steam := filepath.Join(home, ".local", "share", "Steam")
	if info, err := os.Stat(steam); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("Steam dir: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(steam, "steam.sh")); err != nil {
		t.Error("bootstrap not extracted:", err)
	}
	stored, err := os.ReadFile(filepath.Join(steam, "package", "steam_client_ubuntu12.manifest"))
	if err != nil || string(stored) != string(f.manifest) {
		t.Errorf("manifest not stored verbatim: %v", err)
	}
	for name, body := range f.files {
		got, err := os.ReadFile(filepath.Join(steam, "package", name))
		if err != nil || string(got) != string(body) {
			t.Errorf("package %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(steam, "package", "steam_client_ubuntu12.installed")); err == nil {
		t.Error("wrote the .installed marker; extracting is Steam's job")
	}
	// Every created path goes to gamer: .local, .local/share and everything under Steam.
	owned := map[string]bool{}
	for _, c := range *chowns {
		if c.uid != 1000 || c.gid != 1001 {
			t.Errorf("%s handed to %d:%d, want 1000:1001", c.path, c.uid, c.gid)
		}
		owned[c.path] = true
	}
	filepath.Walk(filepath.Join(home, ".local"), func(p string, _ os.FileInfo, _ error) error {
		if !owned[p] {
			t.Errorf("%s not handed to the user", p)
		}
		return nil
	})
}

func TestPreloadSteamOfflineLeavesTheHomeUntouched(t *testing.T) {
	target, home := steamTarget(t, true)
	recordChown(t)
	old := SteamCDN
	SteamCDN = "http://127.0.0.1:1" // nothing listens there
	t.Cleanup(func() { SteamCDN = old })

	var err error
	captureStdout(t, func() { err = PreloadSteam(target, "gamer") })
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("want an offline error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); err == nil {
		t.Error("wrote into the home while offline")
	}
}

func TestPreloadSteamSkipsABadPackageAndKeepsTheRest(t *testing.T) {
	target, home := steamTarget(t, true)
	recordChown(t)
	f := newSteamFixture()
	for name := range f.files {
		if strings.HasPrefix(name, "bins_") {
			f.files[name] = []byte("tampered body, same len!") // wrong hash (and size)
		}
	}
	f.serve(t)

	var err error
	captureStdout(t, func() { err = PreloadSteam(target, "gamer") })
	if err == nil || !strings.Contains(err.Error(), "partly") {
		t.Fatalf("want a partial-preload warning, got %v", err)
	}
	pkg := filepath.Join(home, ".local", "share", "Steam", "package")
	entries, _ := os.ReadDir(pkg)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bins_") {
			t.Errorf("bad package kept as %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(pkg, "public_all.zip.2222222222222222222222222222222222222222")); err != nil {
		t.Error("good package missing:", err)
	}
}

func TestPreloadSteamRemovesTheFolderWhenNoPackageArrives(t *testing.T) {
	target, home := steamTarget(t, true)
	recordChown(t)
	f := newSteamFixture()
	f.files = map[string][]byte{} // manifest only, every package 404s
	f.serve(t)

	var err error
	captureStdout(t, func() { err = PreloadSteam(target, "gamer") })
	if err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "Steam")); err == nil {
		t.Error("left a half-made Steam folder behind")
	}
	if len(f.hits) != 2 {
		t.Errorf("kept downloading after the first failure: %v", f.hits)
	}
}

func TestPreloadSteamSkipsImagesWithoutSteam(t *testing.T) {
	target, home := steamTarget(t, false)
	recordChown(t)
	f := newSteamFixture()
	f.serve(t)

	captureStdout(t, func() {
		if err := PreloadSteam(target, "gamer"); err != nil {
			t.Fatal(err)
		}
	})
	if len(f.hits) != 0 {
		t.Errorf("contacted the CDN for an image without Steam: %v", f.hits)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); err == nil {
		t.Error("wrote into the home")
	}
}

func TestPreloadSteamKeepsAnExistingSteamFolder(t *testing.T) {
	target, home := steamTarget(t, true)
	recordChown(t)
	existing := filepath.Join(home, ".local", "share", "Steam")
	os.MkdirAll(existing, 0o700)
	f := newSteamFixture()
	f.serve(t)

	captureStdout(t, func() {
		if err := PreloadSteam(target, "gamer"); err != nil {
			t.Fatal(err)
		}
	})
	if len(f.hits) != 0 {
		t.Errorf("downloaded although Steam is already there: %v", f.hits)
	}
}

func TestPreloadSteamNeedsTheAccount(t *testing.T) {
	target, _ := steamTarget(t, true)
	recordChown(t)
	if err := PreloadSteam(target, ""); err != nil {
		t.Errorf("no account: %v", err)
	}
	var err error
	captureStdout(t, func() { err = PreloadSteam(target, "nobody") })
	if err == nil {
		t.Error("unknown user accepted")
	}
}
