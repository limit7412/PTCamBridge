package papertracker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWriteCache(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != "127.0.0.1:18080" {
		t.Errorf("cache holds %q, want the bridge address", got)
	}
}

// クライアントはキャッシュされた値に自分で http:// を前置するので、ファイルに
// スキームが入っていると http://http://... になり、黙って接続に失敗する。
func TestWriteCacheRejectsAScheme(t *testing.T) {
	if err := WriteCache(t.TempDir(), "http://127.0.0.1:18080"); err == nil {
		t.Fatal("expected an address with a scheme to be rejected")
	}
}

func TestWriteCacheRejectsABadDirectory(t *testing.T) {
	if err := WriteCache("", "127.0.0.1:1"); err == nil {
		t.Error("expected an empty directory to be rejected")
	}
	if err := WriteCache(filepath.Join(t.TempDir(), "missing"), "127.0.0.1:1"); err == nil {
		t.Error("expected a missing directory to be rejected")
	}

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(file, "127.0.0.1:1"); err == nil {
		t.Error("expected a file to be rejected as an install directory")
	}
}

// バックアップは、クライアントが持っていたカメラのアドレスを保つために存在する。
// 2 回目にも取るとそれをブリッジ自身のアドレスで上書きすることになり、それこそ
// ユーザーが戻したくない値だ。
func TestBackupIsTakenOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	original := "192.168.1.50"
	if err := os.WriteFile(CachePath(dir), []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	backup, err := os.ReadFile(CachePath(dir) + BackupSuffix)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup holds %q, want the camera address %q", backup, original)
	}
}

func TestWriteCacheWithNoExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if _, err := os.Stat(CachePath(dir) + BackupSuffix); !os.IsNotExist(err) {
		t.Error("a backup was created even though there was nothing to preserve")
	}
}

func TestRestoreCache(t *testing.T) {
	dir := t.TempDir()
	original := "192.168.1.50"
	if err := os.WriteFile(CachePath(dir), []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != original {
		t.Errorf("cache holds %q after restore, want %q", got, original)
	}
	if _, err := os.Stat(CachePath(dir) + BackupSuffix); !os.IsNotExist(err) {
		t.Error("the backup should be removed once restored")
	}
}

// 「ここは一度も変更していない」と「復元に失敗した」は区別できなければならない。
// write_cache が切られていればブリッジは起動のたびに復元を試みるので、さもないと
// 一度も触れていない機械で毎回エラーを記録することになる。
func TestRestoreCacheWithoutABackup(t *testing.T) {
	err := RestoreCache(t.TempDir())
	if err == nil {
		t.Fatal("expected an error when there is no backup to restore")
	}
	if !errors.Is(err, ErrNoBackup) {
		t.Errorf("error = %v, want it to wrap ErrNoBackup", err)
	}
}

func TestFindInstallDirReportsNotFound(t *testing.T) {
	// 一時的なホームには PaperTracker が無いので、どの候補にも当たらない。
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	if _, err := FindInstallDir(); err == nil {
		t.Fatal("expected ErrNotFound with no installation present")
	}
}

func TestFindInstallDirRecognisesAnInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	install := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(install, CacheFileName), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := FindInstallDir()
	if err != nil {
		t.Fatalf("FindInstallDir: %v", err)
	}
	if got != install {
		t.Errorf("FindInstallDir() = %q, want %q", got, install)
	}
}

// 初回に保つべきものは無いが、それでもその事実は記録しなければならない。さもないと
// 2 回目はブリッジ自身のアドレスをバックアップしてクライアントのものと称し、
// ブリッジ以前の状態は永久に失われる。
func TestWriteCacheRecordsThatThereWasNoOriginal(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("first WriteCache: %v", err)
	}
	if _, err := os.Stat(path + NoOriginalSuffix); err != nil {
		t.Fatalf("no marker after the first run: %v", err)
	}
	if _, err := os.Stat(path + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a backup was taken when there was no original: %v", err)
	}

	// 2 回目が、ブリッジ自身のアドレスをクライアントのものとして扱ってはいけない。
	if err := WriteCache(dir, "127.0.0.1:18081"); err != nil {
		t.Fatalf("second WriteCache: %v", err)
	}
	if _, err := os.Stat(path + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the second run backed up the bridge's own address: %v", err)
	}

	// 「キャッシュ無し」の復元とは、ファイルを削除することであって、アドレスを
	// 残すことではない。
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		data, _ := os.ReadFile(path)
		t.Errorf("the cache still exists after restoring (%q), want it gone", data)
	}
	if _, err := os.Stat(path + NoOriginalSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the marker outlived the restore")
	}
}

// 通常の場合も当然動かなければならない。実在する元の値が保たれ、そのまま戻される。
func TestWriteCachePreservesARealOriginal(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.WriteFile(path, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if _, err := os.Stat(path + NoOriginalSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the no-original marker was written despite a real original")
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != "192.168.1.50" {
		t.Errorf("restored %q, want the camera address back", got)
	}
}

// write_cache を切ると復元は起動のたびに走り、設定がフォルダを指定していなければ
// 探索も行う。何が書いたとしてもおかしくないほど一般的な名前のバックアップは、
// ブリッジを一度も有効にしていない機械で読み戻されることになる。クライアントの
// キャッシュを見知らぬファイルで置き換え、そのうえでそのファイルを削除していく。
func TestRestoreCacheIgnoresABackupTheBridgeDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	cache := CachePath(dir)
	foreign := cache + ".bak"

	if err := os.WriteFile(cache, []byte("192.168.1.50:80"), 0o644); err != nil {
		t.Fatalf("write the cache: %v", err)
	}
	// 同じフォルダにある、誰か別の人のバックアップ。
	if err := os.WriteFile(foreign, []byte("something else entirely"), 0o644); err != nil {
		t.Fatalf("write the foreign backup: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("RestoreCache = %v, want ErrNoBackup: the bridge wrote nothing here", err)
	}

	got, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("read the cache back: %v", err)
	}
	if string(got) != "192.168.1.50:80" {
		t.Errorf("cache = %q, want it untouched", got)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("the foreign backup was removed: %v", err)
	}
}

// 復元は探索が返したフォルダに対して走るが、PaperTracker は機械に複数あり得る —
// 新しいコピーの隣にある古いコピー。意味があるのはブリッジが書き込んだ方だ。単に
// インストールに見えるだけの最初の 1 つを選ぶと、「復元するものは無い」と報告する
// 一方で、本当に変更されたクライアントは、もう動いていないブリッジを指したまま残る。
func TestFindRestoreDirPrefersTheFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// 先に探索され、まったく普通のインストールでもある。ただし触れられていない。
	untouched := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(untouched, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(CachePath(untouched), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 後から探索される、ブリッジが実際に引き継いだ方。
	changed := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(changed, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(CachePath(changed), []byte("192.168.1.60"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(changed, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	if got, err := FindInstallDir(); err != nil || got != untouched {
		t.Fatalf("FindInstallDir() = %q, %v -- the fixture does not reproduce the ambiguity", got, err)
	}
	got, err := FindRestoreDir()
	if err != nil {
		t.Fatalf("FindRestoreDir: %v", err)
	}
	if got != changed {
		t.Errorf("FindRestoreDir() = %q, want the folder holding the backup %q", got, changed)
	}
}

// 印も同じく数に入る。キャッシュをまったく持たないクライアントに対する初回起動が
// 残すのはそれだけだが、それも同じく「ブリッジがここに居た」ことを示す。
func TestFindRestoreDirFindsAFolderWithOnlyTheMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	got, err := FindRestoreDir()
	if err != nil {
		t.Fatalf("FindRestoreDir: %v", err)
	}
	if got != dir {
		t.Errorf("FindRestoreDir() = %q, want %q", got, dir)
	}
}

// 「復元するものが無い」ことは失敗と区別できなければならない。write_cache を切れば
// 探索は起動のたびに走り、それはブリッジが一度も触れていない機械の上でも同じだ。
func TestFindRestoreDirReportsNoBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	t.Setenv("ProgramFiles(x86)", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	install := filepath.Join(home, "PaperTracker")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(CachePath(install), []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := FindRestoreDir(); !errors.Is(err, ErrNoBackup) {
		t.Errorf("FindRestoreDir error = %v, want ErrNoBackup", err)
	}
}

// キャッシュ自体はその場で上書きせず置き換える。os.WriteFile は書く前にファイルを
// 空にするので、その間にディスクが一杯になるとクライアントにはアドレスがまったく
// 残らない。しかも WriteCache の呼び出し側は失敗をログに書いて先へ進むだけなので、
// それを戻すものが無い。
//
// 違いを可視化するのがハードリンク。元々あったファイルを掴んだままにするので、
// 新しい方が別の名前で作られて rename されたなら古いアドレスのまま読め、古い
// ファイルが切り詰められて上書きされたなら新しいアドレスとして読める。
func TestWriteCacheReplacesTheCacheRatherThanTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "held-open")
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	held, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read the link: %v", err)
	}
	if string(held) != original {
		t.Errorf("the file that was there now reads %q: it was written over in place, not replaced", held)
	}
	if got, err := ReadCache(dir); err != nil || got != "127.0.0.1:18080" {
		t.Errorf("ReadCache() = %q, %v, want the bridge address", got, err)
	}
}

// 復元も同じで、失敗したときはより悪い。直後にバックアップを削除するので、
// 書きかけの元の値だけがユーザーの手元に残ることになる。
func TestRestoreCacheReplacesTheCacheRatherThanTruncatingIt(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	link := filepath.Join(dir, "held-open")
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}

	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}

	held, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read the link: %v", err)
	}
	if string(held) != "127.0.0.1:18080" {
		t.Errorf("the file that was there now reads %q: it was written over in place, not replaced", held)
	}
	if got, err := ReadCache(dir); err != nil || got != original {
		t.Errorf("ReadCache() = %q, %v, want the camera address back", got, err)
	}
}

// write_cache を切ると復元は起動のたびに走るので、二度実行できてはいけない。
// 記録が復元を生き延びた場合 — ロックされたファイルや読み取り専用のせいで削除に
// 失敗した場合 — 次の起動は、その後クライアントがキャッシュしたものの上に
// ブリッジ以前のアドレスを書き戻し、ブリッジを使うのをやめた後にユーザーが選んだ
// カメラを取り消してしまう。
func TestRestoreCacheIsNotAppliedTwice(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.WriteFile(path, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}

	// 削除に失敗した状況の代わり。記録が、進行中の復元が使う名前のもとに戻って
	// いる。削除が失敗したときに残る状態そのもの。
	working := path + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	// クライアントはその後、別のカメラへ移っている。
	if err := os.WriteFile(path, []byte("192.168.1.77"), 0o644); err != nil {
		t.Fatalf("write the new address: %v", err)
	}

	err := RestoreCache(dir)
	if err == nil {
		t.Fatal("a restore that already happened was applied again")
	}
	if !errors.Is(err, ErrRestoreInterrupted) {
		t.Errorf("error = %v, want ErrRestoreInterrupted", err)
	}
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got != "192.168.1.77" {
		t.Errorf("cache = %q, want the address the client chose since", got)
	}
}

// 書けなかった復元は起きていない復元なので、記録は元の場所へ戻さなければならない。
// 確保したままにすると、次の起動はキャッシュと一致しない内容を見つけて推測を拒む。
// ディスク満杯やファイルのロックが、人にしか終わらせられない何かに化ける。
func TestRestoreCachePutsTheRecordBackWhenItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path+BackupSuffix, []byte(original), 0o644); err != nil {
		t.Fatalf("write the backup: %v", err)
	}
	// キャッシュがあるべき場所にディレクトリを置く。そこへの rename は成功し得ず、
	// テストで用意できるものとしてはディスク満杯に最も近い。
	if err := os.MkdirAll(filepath.Join(path, "in-the-way"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := RestoreCache(dir); err == nil {
		t.Fatal("expected the write to fail")
	}

	backup, err := os.ReadFile(path + BackupSuffix)
	if err != nil {
		t.Fatalf("the record was not put back: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %q, want the address it held %q", backup, original)
	}
	if _, err := os.Stat(path + BackupSuffix + RestoringSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the claim was left taken: %v", err)
	}

	// そして道が開けば、誰が何の名前も変えずに復元される。
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("clear the way: %v", err)
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if got, _ := ReadCache(dir); got != original {
		t.Errorf("cache = %q, want the camera address back", got)
	}
}

// その通常版。復元は完了して後片付けだけが失敗したので、クライアントは既に記録の
// 述べるものを持っている。あとは黙って削除するだけ。これは起動のたびに走る。
func TestRestoreCacheCleansUpAfterItself(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	working := path + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte(original), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("RestoreCache = %v, want ErrNoBackup: there is nothing left to put back", err)
	}
	if _, err := os.Stat(working); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leftover was not cleaned up: %v", err)
	}
	if got, _ := ReadCache(dir); got != original {
		t.Errorf("cache = %q, want it left alone", got)
	}
}

// 中断された復元はゴミではなく、これから起きるべき復元。write_cache を戻せば
// キャッシュを再び引き継ぐことになり、作業名のもとにあるアドレスは依然として
// 返すべきもの。だからそれは改めて記録になり、ブリッジ自身のアドレスの写しで
// 置き換えられたりはしない。
func TestWriteCacheReclaimsAnInterruptedRestore(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	original := "192.168.1.50"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	working := path + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte(original), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	backup, err := os.ReadFile(path + BackupSuffix)
	if err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %q, want the client's own address %q", backup, original)
	}
	if _, err := os.Stat(working); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working name outlived the reclaim: %v", err)
	}

	// そしてそこから通常どおり復元される。
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	if got, _ := ReadCache(dir); got != original {
		t.Errorf("cache = %q, want the camera address back", got)
	}
}

// キャッシュをまったく持たなかったクライアントについても同じ。復元とはファイルを
// 削除することであり、それを二度やると、ブリッジが手を引いた後にクライアントが
// 書いたキャッシュを消すことになる。
func TestRestoreCacheDoesNotRemoveACacheWrittenAfterTheRestore(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}

	working := path + NoOriginalSuffix + RestoringSuffix
	if err := os.WriteFile(working, nil, 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	// クライアントはその後、自分のカメラをキャッシュしている。
	if err := os.WriteFile(path, []byte("192.168.1.77"), 0o644); err != nil {
		t.Fatalf("write the new address: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrRestoreInterrupted) {
		t.Fatalf("RestoreCache = %v, want ErrRestoreInterrupted", err)
	}
	if got, err := ReadCache(dir); err != nil || got != "192.168.1.77" {
		t.Errorf("ReadCache() = %q, %v, want the client's own address left alone", got, err)
	}
}

// 復元の途中にあるフォルダも見つけられなければならない。さもないと探索は、
// ブリッジがこの機械に一度も触れていないと報告することになる。
func TestFindRestoreDirFindsAnInterruptedRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, ".local", "share", "PaperTracker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	working := CachePath(dir) + BackupSuffix + RestoringSuffix
	if err := os.WriteFile(working, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}

	got, err := FindRestoreDir()
	if err != nil {
		t.Fatalf("FindRestoreDir: %v", err)
	}
	if got != dir {
		t.Errorf("FindRestoreDir() = %q, want %q", got, dir)
	}
}

// バックアップが数に入るのは完成してからだけ。中途半端なものは無いより悪い。
// backupOnce はそれを見て「ブリッジ以前のアドレスは既に安全だ」と判断するので、
// 本物は上書きされ、復元に使えるのは切り詰められた写しだけになる。
func TestBackupIsNeverVisibleHalfWritten(t *testing.T) {
	dir := t.TempDir()
	cache := CachePath(dir)
	original := "192.168.1.50:80"
	if err := os.WriteFile(cache, []byte(original), 0o644); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	// 一時的な名前のものが放置されておらず、そこにあるバックアップはアドレス全体を
	// 保持している。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	want := map[string]bool{CacheFileName: true, CacheFileName + BackupSuffix: true}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}

	backup, err := os.ReadFile(cache + BackupSuffix)
	if err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if string(backup) != original {
		t.Errorf("backup = %q, want the whole original address %q", backup, original)
	}
}

// write_cache が有効なまま install_dir は変わり得るので、ブリッジは今書き込んで
// いるフォルダと、以前書き込んでいたフォルダの両方に記録を抱えることになる。復元は
// その両方を見つけなければならない。さもないと取り残されたクライアントは、止まった
// ブリッジを指し続ける。
func TestFindRestoreDirsFindsEveryFolderTheBridgeWroteTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first := filepath.Join(home, "PaperTracker")
	second := filepath.Join(home, ".local", "share", "PaperTracker")
	// インストールに見えるが一度も触れていない 3 つ目のフォルダ。
	untouched := filepath.Join(home, ".wine", "drive_c", "PaperTracker")

	for _, dir := range []string{first, second, untouched} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(CachePath(dir), []byte("192.168.1.50"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, dir := range []string{first, second} {
		if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
			t.Fatalf("WriteCache: %v", err)
		}
	}

	dirs, err := FindRestoreDirs()
	if err != nil {
		t.Fatalf("FindRestoreDirs: %v", err)
	}
	if len(dirs) != 2 || !slices.Contains(dirs, first) || !slices.Contains(dirs, second) {
		t.Errorf("FindRestoreDirs() = %q, want exactly %q and %q", dirs, first, second)
	}
}

// install_dir は、探索がまったく知らないフォルダを指定できる — ユーザーの好きな
// 場所に置いたクライアントのポータブルなコピー。その設定が消され、しかも
// [papertracker] セクションを丸ごと削除するのが既定へ戻る方法である以上、変更を
// どこで取り消せばよいかを述べるものは、ブリッジ自身の覚書だけになる。
func TestWrittenDirsRecordsFoldersTheSearchCannotFind(t *testing.T) {
	state := t.TempDir()
	portable := filepath.Join(t.TempDir(), "PaperTracker Portable")

	if dirs, err := WrittenDirs(state); err != nil || len(dirs) != 0 {
		t.Fatalf("WrittenDirs() = %q, %v, want nothing recorded yet", dirs, err)
	}
	if err := RememberWrittenDir(state, portable); err != nil {
		t.Fatalf("RememberWrittenDir: %v", err)
	}
	// 次の起動でまた記録しても、重複してはいけない。
	if err := RememberWrittenDir(state, portable); err != nil {
		t.Fatalf("RememberWrittenDir again: %v", err)
	}

	dirs, err := WrittenDirs(state)
	if err != nil {
		t.Fatalf("WrittenDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != portable {
		t.Errorf("WrittenDirs() = %q, want exactly %q", dirs, portable)
	}

	// 2 つ目のフォルダは、置き換えではなく追加される。
	other := filepath.Join(t.TempDir(), "PaperTracker")
	if err := RememberWrittenDir(state, other); err != nil {
		t.Fatalf("RememberWrittenDir: %v", err)
	}
	dirs, err = WrittenDirs(state)
	if err != nil {
		t.Fatalf("WrittenDirs: %v", err)
	}
	if !slices.Contains(dirs, portable) || !slices.Contains(dirs, other) {
		t.Errorf("WrittenDirs() = %q, want both folders", dirs)
	}
}

// 何も記録されていないことと、記録する先が無いことは、失敗ではなく普通の答え。
// これは起動のたびに走る。
func TestWrittenDirsIgnoresAnEmptyRequest(t *testing.T) {
	if err := RememberWrittenDir("", "/somewhere"); err != nil {
		t.Errorf("RememberWrittenDir with no state directory = %v", err)
	}
	if err := RememberWrittenDir(t.TempDir(), "   "); err != nil {
		t.Errorf("RememberWrittenDir with no install directory = %v", err)
	}
	if dirs, err := WrittenDirs(""); err != nil || dirs != nil {
		t.Errorf("WrittenDirs(\"\") = %q, %v, want nothing", dirs, err)
	}
}

// 作業ファイルの削除は復元の最後の手順であり、単独で失敗する可能性が最も高い。
// その結果残るものは、自身のアドレスが既にキャッシュへ戻っていることを述べなければ
// ならない。作業名のもとにあるだけのファイルは、何も書かないまま終わった復元と
// 区別がつかないからだ。
func TestRestoreCacheMarksALeftoverAsApplied(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.WriteFile(path, []byte("127.0.0.1:18080"), 0o644); err != nil {
		t.Fatalf("write the cache: %v", err)
	}
	// 削除できない記録。中身のあるディレクトリ。印は通常の使い方では空なので、
	// その内容を読むものは無い。
	marker := path + NoOriginalSuffix
	if err := os.MkdirAll(filepath.Join(marker, "in-the-way"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(marker + AppliedSuffix)

	err := RestoreCache(dir)
	if err == nil {
		t.Fatal("expected the removal to be reported")
	}
	// 復元自体は起きた。ブリッジ以前にキャッシュは無かった。
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cache is still there: %v", err)
	}
	if _, err := os.Stat(marker + AppliedSuffix); err != nil {
		t.Errorf("the leftover was not marked as applied: %v", err)
	}
	if _, err := os.Stat(marker + RestoringSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working name is still in use: %v", err)
	}
}

// 適用済みと印の付いた記録はゴミ。それを「クライアントが持っていたもの」として
// 取り戻すと、クライアントが既に離れたアドレスを綴じ込むことになり、次の復元は
// ユーザー自身の選択を取り消す。
func TestWriteCacheDoesNotReclaimAnAppliedRestore(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)

	// 自分のファイルだけ削除できなかった復元が残す状態。
	if err := os.WriteFile(path+BackupSuffix+AppliedSuffix, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	// クライアントはその後、自分のカメラを選んでいる。
	if err := os.WriteFile(path, []byte("192.168.1.77"), 0o644); err != nil {
		t.Fatalf("write the new address: %v", err)
	}

	if err := WriteCache(dir, "127.0.0.1:18080"); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	backup, err := os.ReadFile(path + BackupSuffix)
	if err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if string(backup) != "192.168.1.77" {
		t.Errorf("backup = %q, want the address the client had just before the bridge", backup)
	}
	if _, err := os.Stat(path + BackupSuffix + AppliedSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the applied leftover was kept: %v", err)
	}

	// そして復元が返すのは、古いアドレスではなくユーザーが選んだ方。
	if err := RestoreCache(dir); err != nil {
		t.Fatalf("RestoreCache: %v", err)
	}
	if got, _ := ReadCache(dir); got != "192.168.1.77" {
		t.Errorf("restored %q, want the camera the user chose", got)
	}
}

// write_cache を切れば復元は起動のたびに走るので、同じ残り物に出くわす。戻すものは
// 無く、クライアントのキャッシュには触れてはいけない。
func TestRestoreCacheIgnoresAnAppliedLeftover(t *testing.T) {
	dir := t.TempDir()
	path := CachePath(dir)
	if err := os.WriteFile(path+BackupSuffix+AppliedSuffix, []byte("192.168.1.50"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	if err := os.WriteFile(path, []byte("192.168.1.77"), 0o644); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	if err := RestoreCache(dir); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("RestoreCache = %v, want ErrNoBackup", err)
	}
	if got, _ := ReadCache(dir); got != "192.168.1.77" {
		t.Errorf("cache = %q, want it left alone", got)
	}
	if _, err := os.Stat(path + BackupSuffix + AppliedSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leftover was not cleaned up: %v", err)
	}
}

// 相対の install_dir は、ブリッジがどこから起動されたかによって別のフォルダを
// 意味する。そして記録を読み返すのは、まったく別の場所から始まった後の実行 —
// サインイン時や、アンインストーラがたまたま走る場所からの実行 — だ。
func TestRememberWrittenDirRecordsAnAbsolutePath(t *testing.T) {
	state := t.TempDir()
	install := t.TempDir()

	// t.Chdir ではなく Chdir。このモジュールが対象とする Go のバージョンは
	// t.Chdir より前のもの。ここは並行実行しないので、元に戻す限りプロセス全体への
	// 変更でも安全。
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(filepath.Dir(install)); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("chdir back: %v", err)
		}
	})

	if err := RememberWrittenDir(state, filepath.Base(install)); err != nil {
		t.Fatalf("RememberWrittenDir: %v", err)
	}
	dirs, err := WrittenDirs(state)
	if err != nil {
		t.Fatalf("WrittenDirs: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("WrittenDirs() = %q, want one entry", dirs)
	}
	if !filepath.IsAbs(dirs[0]) {
		t.Errorf("recorded %q, want an absolute path", dirs[0])
	}
	// 実際に書き込んだフォルダを指していなければならない。
	same, err := filepath.EvalSymlinks(dirs[0])
	if err != nil {
		t.Fatalf("resolve the record: %v", err)
	}
	want, err := filepath.EvalSymlinks(install)
	if err != nil {
		t.Fatalf("resolve the install dir: %v", err)
	}
	if same != want {
		t.Errorf("recorded %q, want %q", same, want)
	}
}
