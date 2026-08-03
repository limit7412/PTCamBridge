package tray

import (
	"os/exec"
	"syscall"
)

// createNoWindow は、シェルの補助プロセスがコンソールウィンドウを一瞬表示するのを
// 防ぎます。
const createNoWindow = 0x08000000

// openPath は、ファイル・フォルダ・URL をシェルに渡します。プロトコルハンドラを
// 伴う rundll32 はこの 3 つすべてを扱えます。explorer.exe はパスには適していますが
// URL には扱いづらいのに対して。
func openPath(target string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Start()
}
