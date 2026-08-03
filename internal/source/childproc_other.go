//go:build !windows

package source

import "os/exec"

// configureChildProcess is a no-op away from Windows, where there is no
// console window to hide.
func configureChildProcess(cmd *exec.Cmd) {}
