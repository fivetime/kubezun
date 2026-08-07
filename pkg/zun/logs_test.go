package zun

import "testing"

// The command is sent as one string that Zun splits the way a shell would, so
// an argument containing a space or a quote has to survive the round trip as
// one argument — otherwise `sh -c "echo a b"` silently becomes three.
func TestShellQuoteKeepsArgumentsWhole(t *testing.T) {
	for _, tc := range []struct {
		cmd  []string
		want string
	}{
		{[]string{"echo", "hello"}, `'echo' 'hello'`},
		{[]string{"sh", "-c", "echo a b"}, `'sh' '-c' 'echo a b'`},
		{[]string{"echo", "it's"}, `'echo' 'it'"'"'s'`},
	} {
		if got := shellQuote(tc.cmd); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
