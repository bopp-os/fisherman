package post

import "testing"

func TestWriteFirstRunRefusesBadInput(t *testing.T) {
	cases := []struct{ name, distro, mode string }{
		{"unknown mode", "boppos", "later"},
		{"empty mode", "boppos", ""},
		{"path in distro id", "../etc", "oobe"},
		{"empty distro id", "", "welcome"},
		{"uppercase distro id", "BoppOS", "oobe"},
	}
	for _, c := range cases {
		if err := WriteFirstRun("/nonexistent", c.distro, c.mode); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}
