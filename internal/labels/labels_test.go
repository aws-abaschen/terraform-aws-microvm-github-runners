package labels

import "testing"

func TestMatch(t *testing.T) {
	runner := []string{"self-hosted", "linux", "ARM64", "microvm"}
	req := []string{"microvm"}
	cases := []struct {
		job  []string
		want bool
	}{
		{[]string{"self-hosted", "microvm"}, true},
		{[]string{"Self-Hosted", "MicroVM", "arm64"}, true},
		{[]string{"self-hosted"}, false},    // missing required label
		{[]string{"microvm", "gpu"}, false}, // asks for something we lack
		{[]string{"ubuntu-latest"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := Match(c.job, runner, req); got != c.want {
			t.Errorf("Match(%v) = %v, want %v", c.job, got, c.want)
		}
	}
}

func TestSplit(t *testing.T) {
	got := Split(" a, b ,,c ")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("%v", got)
	}
}
