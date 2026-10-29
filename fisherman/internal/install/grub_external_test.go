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

func TestRenderGrubCfgReadsEntriesAndKeepsStaticFallback(t *testing.T) {
	cfg := renderGrubCfg([]bootEntry{
		{title: "new", linux: "/n/vmlinuz", initrd: []string{"/n/initrd"}, options: "root=UUID=a"},
		{title: "it's old", linux: "/o/vmlinuz", options: "root=UUID=a"},
	}, "1234-ABCD")
	for _, want := range []string{"set default=0", "insmod blsuki\nblscfg\n", "submenu 'Boot entries as installed'",
		"menuentry 'new'", "menuentry 'its old'",
		"search --no-floppy --fs-uuid --set=root 1234-ABCD", "linux /n/vmlinuz root=UUID=a", "initrd /n/initrd"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("grub.cfg is missing %q:\n%s", want, cfg)
		}
	}
	if strings.Index(cfg, "blscfg") > strings.Index(cfg, "submenu") {
		t.Errorf("blscfg must come before the fallback submenu")
	}
	if strings.Index(cfg, "menuentry 'new'") > strings.Index(cfg, "menuentry 'its old'") {
		t.Errorf("entries are not in the order given")
	}
}
