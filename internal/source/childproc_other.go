//go:build !windows

package source

import "os/exec"

// configureChildProcess は Windows 以外では何もしません。隠すべきコンソール
// ウィンドウが存在しないためです。
func configureChildProcess(cmd *exec.Cmd) {}
