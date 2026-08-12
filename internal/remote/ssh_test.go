package remote

import (
	"os/exec"
	"strings"
	"testing"
)

// TestQuoteIsInjectionSafe is the important test in this package.
//
// Every value buidl interpolates into a remote shell command — file paths,
// service names, config values — goes through Quote. A hole here means a
// hostile or merely odd inventory value executes arbitrary code as root on
// every server in the fleet.
//
// Rather than assert on the escaped text, this runs each case through a real
// shell and checks the argument arrives byte-identical.
func TestQuoteIsInjectionSafe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}

	cases := []string{
		"plain",
		"with space",
		"single'quote",
		`double"quote`,
		"semi;colon",
		"pipe|char",
		"amp&ersand",
		"back`tick`",
		"dollar$VAR",
		"${BRACED}",
		"$(command substitution)",
		"new\nline",
		"tab\there",
		"glob*star",
		"question?mark",
		"bracket[abc]",
		"tilde~home",
		"redirect>file",
		"redirect<file",
		"paren(s)",
		"brace{s}",
		"hash#comment",
		"equals=sign",
		"bang!bang",
		"percent%sign",
		"back\\slash",
		// The classic injection attempts.
		"'; rm -rf /; echo '",
		"$(touch /tmp/buidl-pwned)",
		"`touch /tmp/buidl-pwned`",
		"; touch /tmp/buidl-pwned",
		"&& touch /tmp/buidl-pwned",
		"| touch /tmp/buidl-pwned",
		"\\'; whoami; #",
		// Empty must still produce a single argument.
		"",
	}

	for _, input := range cases {
		t.Run(sanitizeTestName(input), func(t *testing.T) {
			// printf %s writes the argument with no interpretation, so any
			// difference in output means the shell reinterpreted our quoting.
			script := "printf %s " + Quote(input)
			out, err := exec.Command("sh", "-c", script).Output()
			if err != nil {
				t.Fatalf("shell rejected the quoted argument: %v\nscript: %s", err, script)
			}
			if string(out) != input {
				t.Errorf("Quote(%q) round-tripped as %q\nscript: %s", input, string(out), script)
			}
		})
	}
}

// TestQuoteProducesSingleArgument verifies quoting does not split a value into
// several arguments, which would silently change a command's meaning.
func TestQuoteProducesSingleArgument(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell available")
	}

	for _, input := range []string{"one two three", "a\tb", "a\nb", "", "*"} {
		// $# reports the argument count the shell actually parsed.
		script := "set -- " + Quote(input) + "; printf %s $#"
		out, err := exec.Command("sh", "-c", script).Output()
		if err != nil {
			t.Fatalf("shell error for %q: %v", input, err)
		}
		if string(out) != "1" {
			t.Errorf("Quote(%q) produced %s arguments, want 1", input, string(out))
		}
	}
}

func TestQuoteEmptyString(t *testing.T) {
	// An empty value must not vanish from the command line, which would shift
	// every later positional argument.
	if got := Quote(""); got != "''" {
		t.Errorf("Quote(\"\") = %q, want ''", got)
	}
}

func TestQuoteLeavesSafeValuesReadable(t *testing.T) {
	// Commands appear in logs and error messages, so unnecessary quoting hurts
	// legibility.
	for _, safe := range []string{"k3s", "/etc/rancher/k3s/config.yaml", "0600", "systemctl"} {
		if got := Quote(safe); got != safe {
			t.Errorf("Quote(%q) = %q, want it unquoted", safe, got)
		}
	}
}

// TestCommandErrorIncludesStderr checks that a remote failure explains itself.
func TestCommandErrorIncludesStderr(t *testing.T) {
	err := &CommandError{
		Command:  "systemctl start k3s",
		Host:     "10.0.0.1",
		ExitCode: 1,
		Stderr:   "Job for k3s.service failed.\nSee 'journalctl -xe'.",
	}

	msg := err.Error()
	for _, want := range []string{"10.0.0.1", "exit 1", "systemctl start k3s", "journalctl"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should contain %q, got:\n%s", want, msg)
		}
	}
}

func TestCommandErrorWithoutStderr(t *testing.T) {
	err := &CommandError{Command: "true", Host: "h", ExitCode: 3}
	if strings.HasSuffix(err.Error(), "\n") {
		t.Errorf("error should not end with a newline when stderr is empty: %q", err.Error())
	}
}

func TestExpandHome(t *testing.T) {
	// An unexpanded ~ would be treated as a literal directory name and fail to
	// open with a confusing error.
	got := expandHome("~/.ssh/id_ed25519")
	if strings.HasPrefix(got, "~") {
		t.Errorf("expandHome did not expand: %q", got)
	}
	if !strings.HasSuffix(got, "/.ssh/id_ed25519") {
		t.Errorf("expandHome = %q", got)
	}

	// Absolute and relative paths pass through untouched.
	for _, path := range []string{"/etc/keys/id", "relative/key"} {
		if got := expandHome(path); got != path {
			t.Errorf("expandHome(%q) = %q, want unchanged", path, got)
		}
	}
}

// sanitizeTestName makes an input safe and readable as a subtest name.
func sanitizeTestName(s string) string {
	if s == "" {
		return "empty"
	}
	replaced := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '/':
			return '_'
		}
		return r
	}, s)
	if len(replaced) > 30 {
		replaced = replaced[:30]
	}
	return replaced
}
