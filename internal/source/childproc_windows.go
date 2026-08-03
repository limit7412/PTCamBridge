package source

import (
	"os/exec"
	"syscall"
)

// createNoWindow は、PTCamBridge 自身がコンソール無しで動いている機械で、ffmpeg が
// コンソールウィンドウを一瞬表示するのを防ぎます。
const createNoWindow = 0x08000000

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
