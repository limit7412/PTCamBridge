package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/papertracker"
)

// ワイルドカードの bind は、listen する対象としては妥当で、クライアントのアドレス
// キャッシュに書く値としては役に立たない。
func TestConnectAddress(t *testing.T) {
	cases := map[string]string{
		"[::]:18080":       "127.0.0.1:18080",
		"0.0.0.0:18080":    "127.0.0.1:18080",
		"127.0.0.1:18080":  "127.0.0.1:18080",
		"192.168.1.5:8080": "192.168.1.5:8080",
		"[::1]:18080":      "[::1]:18080",
		"not-an-address":   "not-an-address",
	}
	for addr, want := range cases {
		if got := connectAddress(addr); got != want {
			t.Errorf("connectAddress(%q) = %q, want %q", addr, got, want)
		}
	}
}

// -restore-cache はアンインストールのためのもので、そのときフォルダを指す設定は
// たいてい既に無い。だからどのクライアントを戻すかは探索が決める。機械に 2 つ
// インストールがあるなら、それはブリッジが変更した方でなければならず、先に来た方
// ではない。もう一方を復元すれば、何も起きていないフォルダに対して成功を報告し、
// 本物のキャッシュは撤去されつつあるブリッジを指したまま残る。
func TestRestoreDirPicksTheFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	untouched := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(untouched, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(untouched), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	changed := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(changed, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := papertracker.WriteCache(changed, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	// 設定ファイルが無い。このフラグが存在する理由そのものの状況。
	opts := options{configPath: filepath.Join(t.TempDir(), "gone.toml")}
	if err := restoreCache(opts); err != nil {
		t.Fatalf("restoreCache: %v", err)
	}

	if _, err := os.Stat(papertracker.CachePath(changed)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the folder the bridge wrote to was not restored: %v", err)
	}
	if got, err := papertracker.ReadCache(untouched); err != nil || got != "192.168.1.50" {
		t.Errorf("the untouched folder = %q, %v, want it left alone", got, err)
	}
}

// 設定は、ブリッジが一度も書いていないフォルダを指し得る。write_cache が有効な
// まま、クライアントが別の場所へ再インストールされ、install_dir がそれを追った
// 場合。記録は古いフォルダに残り、そちらのクライアントを、間もなく止まるブリッジへ
// 向けたままにする。だから「ここにバックアップは無い」で終わらせてはいけない。
func TestRestoreFallsBackWhenTheNamedFolderHasNoBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// ブリッジが実際に書いた場所であり、もう見ていない場所。
	old := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(old), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := papertracker.WriteCache(old, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	// 設定が今指している場所。
	moved := t.TempDir()
	if err := os.WriteFile(papertracker.CachePath(moved), []byte("192.168.1.60"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	restored, err := restoreEverywhereItWas(moved)
	if err != nil {
		t.Fatalf("restoreEverywhereItWas: %v", err)
	}
	if len(restored) != 1 || restored[0] != old {
		t.Errorf("restored %q, want just the folder holding the backup %q", restored, old)
	}
	if got, err := papertracker.ReadCache(old); err != nil || got != "192.168.1.50" {
		t.Errorf("the folder the bridge wrote to = %q, %v, want the camera address back", got, err)
	}
	if got, err := papertracker.ReadCache(moved); err != nil || got != "192.168.1.60" {
		t.Errorf("the folder the settings name = %q, %v, want it left alone", got, err)
	}
}

// write_cache が有効なまま install_dir は変わり得る — クライアントが移動されたり
// 再インストールされたり — その場合ブリッジは両方のフォルダに記録を残す。最初の
// 1 つを復元したところで止めると、もう一方のクライアントは撤去されつつあるブリッジを
// 指したまま残り、二度と見に来るものは無い。
func TestRestoreUndoesEveryFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first := filepath.Join(home, "PaperTracker")
	second := filepath.Join(home, ".local", "share", "PaperTracker")
	for dir, address := range map[string]string{first: "192.168.1.50", second: "192.168.1.60"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(papertracker.CachePath(dir), []byte(address), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := papertracker.WriteCache(dir, "127.0.0.1:18080"); err != nil {
			t.Fatalf("WriteCache: %v", err)
		}
	}

	restored, err := restoreEverywhereItWas(second)
	if err != nil {
		t.Fatalf("restoreEverywhereItWas: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("restored %q, want both folders", restored)
	}
	if got, err := papertracker.ReadCache(first); err != nil || got != "192.168.1.50" {
		t.Errorf("the folder no longer named = %q, %v, want its own camera back", got, err)
	}
	if got, err := papertracker.ReadCache(second); err != nil || got != "192.168.1.60" {
		t.Errorf("the folder the settings name = %q, %v, want its own camera back", got, err)
	}
}

// 他人のアプリケーションに対してブリッジがしたことの取り消しが、設定ファイル全体の
// 妥当性に依存してはいけない。さもないと、綴りを誤ったキーや範囲外の値を抱えたまま
// PTCamBridge をアンインストールする人は、クライアントが撤去中のブリッジを指したまま
// 「何も変更されていない」と告げられる。
func TestRestoreReadsInstallDirFromAnOtherwiseUnusableSettingsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	// 探索が見に行かない場所。
	install := filepath.Join(t.TempDir(), "PaperTracker Portable")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(install), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := papertracker.WriteCache(install, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	cfgPath := filepath.Join(t.TempDir(), "ptcambridge.toml")
	settings := "[server]\nlsiten = 'oops'\n\n[papertracker]\ninstall_dir = '" + install + "'\n"
	if err := os.WriteFile(cfgPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("write the settings: %v", err)
	}

	opts := options{configPath: cfgPath}
	if got := configuredInstallDir(opts); got != install {
		t.Fatalf("configuredInstallDir() = %q, want %q even though the file has a bad key", got, install)
	}
	if err := restoreCache(opts); err != nil {
		t.Fatalf("restoreCache: %v", err)
	}
	if got, err := papertracker.ReadCache(install); err != nil || got != "192.168.1.50" {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// フォルダは環境変数だけで指定され得るし、ブリッジが書き込むフォルダは、ブリッジが
// 取り消せなければならないフォルダでもある。ここでファイルだけを読むと、実際に
// 変更された唯一のクライアントについて「何も変更されていない」と報告することになる。
func TestRestoreHonoursTheInstallDirEnvironmentVariable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	// 探索が見に行かない場所で、変数だけがそれを指している。
	install := filepath.Join(t.TempDir(), "PaperTracker Portable")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(install), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := papertracker.WriteCache(install, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	t.Setenv(config.EnvInstallDir, install)

	// 設定ファイルがまったく無い。アンインストールの後の状態。
	opts := options{configPath: filepath.Join(t.TempDir(), "gone.toml")}
	if got := configuredInstallDir(opts); got != install {
		t.Fatalf("configuredInstallDir() = %q, want the folder from %s", got, config.EnvInstallDir)
	}
	if err := restoreCache(opts); err != nil {
		t.Fatalf("restoreCache: %v", err)
	}
	if got, err := papertracker.ReadCache(install); err != nil || got != "192.168.1.50" {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// 既定へ戻すとは [papertracker] セクションを削除することであり、それは install_dir も
// 一緒に持っていく。それが探索の覆わないフォルダを指していた場合、それに言及する
// ものは二度と現れない。だからブリッジは、どこへ書いたかについて自分の覚書を持つ。
func TestRestoreFindsAFolderNoSettingNamesAnyMore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", filepath.Join(home, "config"))

	// クライアントのポータブルなコピー。探索が決して見に行かない場所にある。
	portable := filepath.Join(t.TempDir(), "PaperTracker Portable")
	if err := os.MkdirAll(portable, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(papertracker.CachePath(portable), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// write_cache を有効にした実行がすること。
	if err := papertracker.WriteCache(portable, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if err := rememberWrittenDir(portable); err != nil {
		t.Fatalf("rememberWrittenDir: %v", err)
	}

	// その後ユーザーが [papertracker] セクションを丸ごと削除するので、そのフォルダを
	// 指すものは無くなる。
	restored, err := restoreEverywhereItWas("")
	if err != nil {
		t.Fatalf("restoreEverywhereItWas: %v", err)
	}
	if len(restored) != 1 || restored[0] != portable {
		t.Errorf("restored %q, want the folder the bridge recorded %q", restored, portable)
	}
	if got, err := papertracker.ReadCache(portable); err != nil || got != "192.168.1.50" {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// どこにも何も無いことは失敗ではなく普通の答えであり、コマンドはエラーを報告せず
// そう述べる。
func TestRestoreCacheSaysThereIsNothingToUndo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	opts := options{configPath: filepath.Join(t.TempDir(), "gone.toml")}
	if restored, err := restoreEverywhereItWas(""); err != nil || len(restored) != 0 {
		t.Fatalf("restoreEverywhereItWas() = %q, %v, want nothing to do and no error", restored, err)
	}
	if err := restoreCache(opts); err != nil {
		t.Errorf("restoreCache = %v, want it to report that there is nothing to restore", err)
	}
}
