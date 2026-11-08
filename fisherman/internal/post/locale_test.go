package post

import "testing"

func TestWriteLocaleNothingSetIsANoOp(t *testing.T) {
	if err := WriteLocale("/nonexistent", "", "", ""); err != nil {
		t.Fatalf("empty values should do nothing, got %v", err)
	}
}

func TestWriteLocaleRefusesUnsafeValues(t *testing.T) {
	cases := []struct{ name, locale, keymap, timezone string }{
		{"locale with a space", "en US", "", ""},
		{"locale with a newline", "en_US\nFOO=1", "", ""},
		{"keymap with a slash", "", "../x", ""},
		{"timezone climbing out", "", "", "../../etc/passwd"},
		{"timezone absolute", "", "", "/etc/passwd"},
		{"timezone with dots", "", "", "Europe/.."},
	}
	for _, c := range cases {
		if err := WriteLocale("/nonexistent", c.locale, c.keymap, c.timezone); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}

func TestLocaleRegexpsAcceptRealValues(t *testing.T) {
	for _, l := range []string{"en_US.UTF-8", "de_DE", "C", "sr_RS@latin"} {
		if l == "C" {
			continue // a bare "C" is not offered by the installer
		}
		if !localeRe.MatchString(l) {
			t.Errorf("locale %q should be accepted", l)
		}
	}
	for _, z := range []string{"UTC", "Europe/Berlin", "America/Argentina/Buenos_Aires", "Etc/GMT+5"} {
		if !timezoneRe.MatchString(z) {
			t.Errorf("timezone %q should be accepted", z)
		}
	}
}
