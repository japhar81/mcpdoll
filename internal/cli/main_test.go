// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestMain scrubs MCPDOLL_* from the environment.
//
// The CLI reads configuration from the environment by design, which means a
// developer who exports MCPDOLL_API_URL to point at their own stack would
// otherwise see tests pass or fail based on their shell. Scrubbing once here is
// safe with t.Parallel, whereas t.Setenv is not.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		for i := range kv {
			if kv[i] == '=' {
				if len(kv) > 8 && kv[:8] == "MCPDOLL_" {
					os.Unsetenv(kv[:i])
				}
				break
			}
		}
	}
	os.Exit(m.Run())
}

// runCLI executes the command tree in-process and returns stdout, stderr, and
// the exit code the binary would have returned.
//
// It goes through [Execute] rather than calling cobra directly, so the exit-code
// mapping is exercised too: a command that returns the wrong error type is a
// real defect, and testing the happy path through a different door would hide it.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	opts := Options{
		Stdout: &out,
		Stderr: &errOut,
		// A path inside a fresh temp dir that deliberately does not exist: the
		// loader treats absence as first-run, so tests never read the
		// developer's ~/.mcpdoll/config.yaml.
		ConfigPath: filepath.Join(t.TempDir(), "absent-config.yaml"),
	}

	root := New(opts)
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		return out.String(), errOut.String(), ExitOK
	}
	errOut.WriteString("mcpdoll: " + err.Error() + "\n")
	return out.String(), errOut.String(), codeFor(err)
}
