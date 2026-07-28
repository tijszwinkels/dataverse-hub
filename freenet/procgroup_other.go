//go:build !unix

package freenet

import "os/exec"

// configureProcessGroup is a no-op off Unix: exec.CommandContext's default
// cancellation (kill the direct child) is all that is portable there.
func configureProcessGroup(cmd *exec.Cmd) {}
