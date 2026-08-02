package source

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps ffmpeg from flashing a console window on a machine
// where PaperBridge itself runs without one.
const createNoWindow = 0x08000000

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
