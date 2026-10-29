package install

import (
	"strings"
	"testing"
)

const sampleEntry = `title Arch Linux (ostree:0)
version 2
options root=UUID=abc rw ostree=/ostree/boot.1/default/x/0
linux /ostree/default-x/vmlinuz-7
initrd /ostree/default-x/initramfs-7.img
`

func TestParseBootEntry(t *testing.T) {
	e := parseBootEntry(sampleEntry)
	if e.title != "Arch Linux (ostree:0)" || e.version != "2" || e.linux != "/ostree/default-x/vmlinuz-7" {
		t.Errorf("unexpected entry: %+v", e)
	}
	if len(e.initrd) != 1 || !strings.Contains(e.options, "ostree=/ostree/boot.1") {
		t.Errorf("unexpected initrd or options: %+v", e)
	}
}

func TestRenderGrubCfgListsEveryEntry(t *testing.T) {
	cfg := renderGrubCfg([]bootEntry{
		{title: "new", linux: "/n/vmlinuz", initrd: []string{"/n/initrd"}, options: "root=UUID=a"},
		{title: "it's old", linux: "/o/vmlinuz", options: "root=UUID=a"},
	}, "1234-ABCD")
	for _, want := range []string{"set default=0", "menuentry 'new'", "menuentry 'its old'",
		"search --no-floppy --fs-uuid --set=root 1234-ABCD", "linux /n/vmlinuz root=UUID=a", "initrd /n/initrd"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("grub.cfg is missing %q:\n%s", want, cfg)
		}
	}
	if strings.Index(cfg, "menuentry 'new'") > strings.Index(cfg, "menuentry 'its old'") {
		t.Errorf("entries are not in the order given")
	}
}

func TestSetINIValue(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"replaces the key", "[core]\nrepo_version=1\n[sysroot]\nbootloader=none\nreadonly=true\n",
			"[core]\nrepo_version=1\n[sysroot]\nbootloader=grub2\nreadonly=true\n"},
		{"adds the key to the section", "[sysroot]\nreadonly=true\n[core]\nx=1\n",
			"[sysroot]\nreadonly=true\nbootloader=grub2\n[core]\nx=1\n"},
		{"adds the key at the end of the last section", "[core]\nx=1\n[sysroot]\nreadonly=true\n",
			"[core]\nx=1\n[sysroot]\nreadonly=true\nbootloader=grub2\n"},
		{"adds the section", "[core]\nx=1\n", "[core]\nx=1\n[sysroot]\nbootloader=grub2\n"},
		{"empty file", "", "[sysroot]\nbootloader=grub2\n"},
	}
	for _, c := range cases {
		if got := setINIValue(c.in, "sysroot", "bootloader", "grub2"); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
