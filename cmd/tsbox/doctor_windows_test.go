package main

import "testing"

func TestAppDataRel(t *testing.T) {
	local, roaming := `C:\Users\u\AppData\Local`, `C:\Users\u\AppData\Roaming`
	cases := []struct{ state, rel, sub string }{
		{`C:\Users\u\AppData\Local\tsbox`, "tsbox", "Local"},
		{`C:\Users\u\AppData\Roaming\x\tsbox`, `x\tsbox`, "Roaming"},
		{`C:\Users\u\AppData\Local\Temp\tsbox`, "", ""},
		{`C:\Users\u\AppData\Local\Packages\p\tsbox`, "", ""},
		{`C:\tsbox-state`, "", ""},
		{`D:\tsbox`, "", ""},
		{`C:\Users\u\AppData\Local`, "", ""},
	}
	for _, c := range cases {
		if rel, sub := appDataRel(c.state, local, roaming); rel != c.rel || sub != c.sub {
			t.Errorf("%s: got %q %q, want %q %q", c.state, rel, sub, c.rel, c.sub)
		}
	}
}
