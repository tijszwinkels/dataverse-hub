//go:build unix

package freenet

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the publish command in its own process group and
// makes cancellation SIGKILL that whole group.
//
// exec.CommandContext's default cancel only signals the direct child. The
// publisher is a bash script that shells out to node/fdev, so the default
// would reap the script and leave its children running — still talking to
// Freenet, still holding the timeout we just decided to give up on.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// A negative PID signals the process group led by that PID.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
