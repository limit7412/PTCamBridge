//go:build !windows

package papertracker

import (
	"os"
	"path/filepath"
)

// candidateDirs は、Windows 以外のビルドが目にし得る Wine や開発時の配置を
// 想定しています。クライアント自体は Windows 版しか配布されていないので、これは
// パッケージをテスト可能にし、コード経路をプラットフォーム間で同一に保つために
// 存在します。
func candidateDirs() []string {
	var dirs []string
	home, err := os.UserHomeDir()
	if err != nil {
		return dirs
	}
	return append(dirs,
		filepath.Join(home, "PaperTracker"),
		filepath.Join(home, ".local", "share", "PaperTracker"),
		filepath.Join(home, ".wine", "drive_c", "PaperTracker"),
		filepath.Join(home, ".wine", "drive_c", "Program Files", "PaperTracker"),
	)
}
