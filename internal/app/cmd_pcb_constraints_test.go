package app

import "testing"

// splitNets is the only place a user-typed net list becomes a payload, so the
// trimming rules matter: EasyEDA net names are case-sensitive and can contain
// characters we must not normalize away (+3V3, USB_DP, C6_N3), while shell
// quoting routinely leaves stray spaces around the commas.
func TestSplitNets(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "A0,A1,A2", []string{"A0", "A1", "A2"}},
		{"spaces around commas", " A0 , A1 ,A2 ", []string{"A0", "A1", "A2"}},
		{"single net", "USB_DP", []string{"USB_DP"}},
		{"empty segments dropped", "A0,,A1,", []string{"A0", "A1"}},
		{"case and sign preserved", "+3V3,gnd,C6_N3", []string{"+3V3", "gnd", "C6_N3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitNets(tc.in)
			if err != nil {
				t.Fatalf("splitNets(%q) errored: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("splitNets(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitNets(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// An all-blank list must be refused rather than silently sending an empty nets
// array to the platform (which would create a constraint that constrains nothing).
func TestSplitNetsRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", ",", " , , "} {
		if got, err := splitNets(in); err == nil {
			t.Errorf("splitNets(%q) = %v, want an error", in, got)
		}
	}
}
