// Copyright 2026 Henry Zektser.

package snapshot

import "os"

// writeFile is a terse test helper for fixture files.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content+"\n"), 0o600)
}
