// Package papertracker は、シリアル接続のカメラが無いときに PaperTracker
// クライアントが頼るアドレスキャッシュを書きます。
//
// クライアントは裸のアドレスが 1 行だけ入ったファイルを読み、自分で http:// を
// 前置します。つまりブリッジを指させるには 1 行書けば済みます。
package papertracker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CacheFileName は、クライアントの実行ファイルの隣に置かれるアドレスキャッシュです。
const CacheFileName = "wifi_cache.txt"

// BackupSuffix は、ブリッジが引き継ぐ前にクライアントが持っていたアドレスを
// 保存するために付ける接尾辞です。
//
// 名前に PTCamBridge が入っているのは意図的です。write_cache を切ると復元は起動の
// たびに走りますし、設定がフォルダを指定していなければ探索も行います。単なる
// ".bak" では、ブリッジを一度も有効にしたことのない機械でそれを読み戻すことに
// なります。クライアントが持っていたものを、誰か別の人が置いたファイルで上書きし、
// そのうえでそのファイルを削除してしまいます。「これはブリッジが置いた」ことを
// 表せるのは、他の誰も選ばない名前だけです。
const BackupSuffix = ".ptcambridge-backup"

// NoOriginalSuffix は、ブリッジが最初に書いた時点でクライアントがキャッシュ済みの
// アドレスをまったく持っていなかったことを示します。
//
// これが無いと初回は記録を残さないので、2 回目はそこにあるブリッジ自身のアドレスを
// 見つけ、それを「元の値」として保存します。復元はユーザーに、始まりの状態ではなく
// ブリッジを返すことになり、「キャッシュが無い」状態へ戻る手段も失われます。
const NoOriginalSuffix = ".ptcambridge-backup.none"

// RestoringSuffix は、2 種類の記録のうち今まさに戻している方に、その作業のあいだ
// だけ付ける接尾辞です。
//
// write_cache を切ると復元は起動のたびに試みられるので、二度実行できてはいけません。
// 記録はキャッシュを書く前にこの名前へ移し、書き終えたら削除します。最後の削除が
// 失敗すればゴミが残ることはありますが、復元がもう一度走って、その後クライアントが
// キャッシュしたアドレスの上にブリッジ以前のアドレスを書き戻すことは決してありません。
//
// 途中で中断された復元もこの名前のもとで認識され、次の起動で完了します。移動は
// 書き込みより前に起きるので、ここで見つかった記録はまだ適用されていない可能性が
// あるからです。
const RestoringSuffix = ".restoring"

// AppliedSuffix は、そのアドレスが既にクライアントのキャッシュへ戻っており、あとは
// 削除を待つだけの記録に付けます。
//
// 作業ファイルの削除は復元の最後の手順であり、単独で失敗する可能性が最も高い手順
// でもあります。スキャナに掴まれたファイル、読み取り専用になったフォルダ。そうして
// 残ったものは、何も書かないうちに止まった復元とまったく同じに見えます。しかし
// この 2 つは正反対の対応を求めます。一方はゴミ、もう一方はクライアント自身の
// アドレスであり、その唯一の写しです。推測せず名前を変えることが、両者を分けています。
const AppliedSuffix = ".applied"

// ErrNotFound は、PaperTracker のインストール先が見つからなかったことを表します。
var ErrNotFound = errors.New("papertracker: no installation directory found")

// CachePath は、インストールディレクトリ内のキャッシュファイルです。
func CachePath(installDir string) string {
	return filepath.Join(installDir, CacheFileName)
}

// WriteCache は、クライアントのキャッシュを addr へ向けます。addr は
// "127.0.0.1:18080" のような裸の host:port でなければなりません。
//
// 元のファイルは初回だけ wifi_cache.txt.bak へ複製します。初回だけなのは、毎回
// バックアップを取るとすぐに、カメラのアドレスではなくブリッジ自身のアドレスを
// 抱えることになるからです。ユーザーが戻したい値は前者です。
func WriteCache(installDir, addr string) error {
	if strings.TrimSpace(installDir) == "" {
		return errors.New("papertracker: install directory is empty")
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("papertracker: address is empty")
	}
	if strings.Contains(addr, "://") {
		return fmt.Errorf("papertracker: address %q must be a bare host:port, the client adds the scheme", addr)
	}
	info, err := os.Stat(installDir)
	if err != nil {
		return fmt.Errorf("papertracker: install directory %q: %w", installDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("papertracker: install directory %q is not a directory", installDir)
	}

	path := CachePath(installDir)
	if err := backupOnce(path); err != nil {
		return err
	}
	// その場で上書きせず置き換える。os.WriteFile はまず切り詰めるので、その間に
	// ディスクが一杯になると、クライアントには空か書きかけのアドレスが残る。しかも
	// 呼び出し側は失敗をログに書いて先へ進むだけなので、それを直すものが無い。
	// rename なら、起きるか起きないかのどちらかで済む。
	if err := writeAtomic(path, []byte(addr)); err != nil {
		return err
	}
	return nil
}

// backupOnce は、ブリッジ以前の状態を一度だけ記録します。元のファイルを
// path+BackupSuffix へ複製するか、元のファイルが無かったことを示す印を置くかの
// どちらかです。
func backupOnce(path string) error {
	backup := path + BackupSuffix
	marker := path + NoOriginalSuffix

	// どちらのファイルがあっても、ブリッジ以前の状態は既にディスク上にあるという
	// こと。2 度目はそれをブリッジ自身のアドレスで上書きするだけになる。
	for _, existing := range []string{backup, marker} {
		switch found, err := exists(existing); {
		case err != nil:
			return err
		case found:
			return nil
		}
	}

	// 復元の作業名のもとに残されたファイルは 2 つのうちどちらかであり、両者は
	// 正反対の対応を求める。RestoreCache が問うのと同じ問い。
	//
	// 復元が完了していないなら、そのファイルはブリッジ以前にクライアントが持って
	// いたものをまだ保持していて、しかも今まさにブリッジがキャッシュを再び引き継ごう
	// としている。それは改めて記録になる。中断された復元がゴミではなく回復可能である
	// のは、これによる。
	//
	// 復元は完了していて後片付けだけが失敗したのなら、その中のアドレスは既に
	// キャッシュへ戻っている。そしてクライアントはその後、自分のカメラへ移っている
	// かもしれない。そこでそれを取り戻すと、古くなったアドレスを「クライアントが
	// 持っていたもの」として綴じ込むことになり、次の復元はユーザー自身の選択を
	// 取り消してしまう。それはゴミであり、残すべき記録は、今キャッシュが述べている
	// ものの新しい写しの方だ。
	for _, record := range []struct {
		name  string
		erase bool
	}{
		{backup, false},
		{marker, true},
	} {
		// 復元自身によって適用済みと印が付いている。その中のアドレスはクライアントが
		// 今持っているものか、クライアントが別のものを選ぶまで持っていたもの。
		// いずれにせよもうブリッジ以前の状態ではないので、残すべき記録は、今
		// キャッシュが述べているものの新しい写し。
		dropped, err := dropApplied(record.name)
		if err != nil {
			return err
		}
		if dropped {
			break
		}

		claimed := record.name + RestoringSuffix
		switch found, err := exists(claimed); {
		case err != nil:
			return err
		case !found:
			continue
		}
		// 印が無いので、何も書かないまま終わった復元かもしれない。決め手になるのは、
		// クライアントが記録の述べるものをそのまま持っているかどうか。それ以外は
		// 未適用として扱う。その方向で間違えた場合の代償は古い記録が残ることであり、
		// クライアント自身のアドレスの唯一の写しを失うことではないから。
		done, err := restoreLooksDone(path, claimed, record.erase)
		if err != nil {
			return err
		}
		if done {
			if err := os.Remove(claimed); err != nil {
				return fmt.Errorf("papertracker: remove %s: %w", claimed, err)
			}
			break
		}
		if err := os.Rename(claimed, record.name); err != nil {
			return fmt.Errorf("papertracker: rename %s: %w", claimed, err)
		}
		return nil
	}

	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// クライアントはキャッシュ済みのアドレスを持っていなかった。それを記録する
		// ことが、この状態を復元可能にしている。
		if err := writeAtomic(marker, nil); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	// 一時的な名前で書いてから rename するので、バックアップは常に完全な形でしか
	// 存在しない。書きかけのものは無いより悪い。上の検査はそれを見て「ブリッジ以前の
	// 状態は既に安全だ」と判断し、復元は切り詰められたアドレスを返すことになる。
	if err := writeAtomic(backup, original); err != nil {
		return err
	}
	return nil
}

// writeAtomic は、同じディレクトリの一時ファイルを経由して data を path へ書きます。
// 読み手がその名前のもとで不完全なファイルを目にすることはありません。
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("papertracker: create a temporary file for %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("papertracker: write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("papertracker: close %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("papertracker: chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("papertracker: rename onto %s: %w", path, err)
	}
	return nil
}

// ErrNoBackup は、戻すものが無いことを表します。ブリッジがこのキャッシュを一度も
// 書いていないか、既に復元済みかのどちらかです。起動のたびに復元を試みる呼び出し側は、
// これで「失敗した復元」と区別します。
var ErrNoBackup = errors.New("papertracker: no backup to restore")

// ErrRestoreInterrupted は、復元が途中で止まり、ブリッジ以前のアドレスが作業名の
// もとに残っていることを表します。これを自動では適用しないのは意図的です。次の
// 起動でそれをやると、クライアントがその後更新したキャッシュを上書きすることに
// なります。
var ErrRestoreInterrupted = errors.New("papertracker: a restore was interrupted")

// RestoreCache は、ブリッジ以前のアドレスを戻し、その記録を削除します。PTCamBridge
// を使うのをやめたユーザーが、クライアントを自分のカメラへ戻せるようにするためです。
func RestoreCache(installDir string) error {
	path := CachePath(installDir)

	working, erase, err := claimRestore(path)
	if err != nil {
		return err
	}

	if err := applyRestore(path, working, erase); err != nil {
		// 何も戻せなかったので、確保も一緒に戻す。確保したままにすると、ディスク
		// 満杯やファイルのロックが「人にしか終わらせられない復元」に化ける。次の
		// 起動は、内容がキャッシュと一致しない記録を見つけて推測を拒む。元の場所に
		// 戻しておけば、単にもう一度試されるだけで済む。
		if undo := unclaimRestore(working); undo != nil {
			return errors.Join(err, undo)
		}
		return err
	}

	if err := os.Remove(working); err != nil {
		// クライアントは既に出発点へ戻っているので、残っているのはブリッジ自身の
		// ファイルだけ。ただし作業名のもとにあるファイルは曖昧で、次の起動はその
		// 中のアドレスが適用済みかどうかを推測しなければならなくなる。名前でそう
		// 述べることが、その推測を取り除く。それすら失敗したなら他に試せることは
		// 無く、いずれにせよ呼び出し側には伝える。
		applied := strings.TrimSuffix(working, RestoringSuffix) + AppliedSuffix
		if renameErr := os.Rename(working, applied); renameErr != nil {
			return errors.Join(
				fmt.Errorf("papertracker: %s was restored but %s could not be removed: %w", path, working, err),
				fmt.Errorf("papertracker: rename %s to %s: %w", working, applied, renameErr),
			)
		}
		return fmt.Errorf("papertracker: %s was restored but %s could not be removed, so it was left as %s: %w",
			path, working, applied, err)
	}
	return nil
}

// dropApplied は、既に戻し終えた記録を削除し、そもそも在ったかどうかを返します。
// 判断すべきことは何もありません。その中のアドレスがクライアントの持っているもので
// あることは、名前が述べています。
func dropApplied(recordName string) (bool, error) {
	applied := recordName + AppliedSuffix
	switch found, err := exists(applied); {
	case err != nil:
		return false, err
	case !found:
		return false, nil
	}
	if err := os.Remove(applied); err != nil {
		return false, fmt.Errorf("papertracker: remove %s: %w", applied, err)
	}
	return true, nil
}

// applyRestore は、確保した記録が述べるとおりにクライアントを戻します。
func applyRestore(path, working string, erase bool) error {
	if erase {
		// ブリッジ以前にキャッシュは無かったので、それを戻すとは、空のファイルを
		// 書くことではなくファイルを削除すること。
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("papertracker: remove %s: %w", path, err)
		}
		return nil
	}
	original, err := os.ReadFile(working)
	if err != nil {
		return fmt.Errorf("papertracker: read %s: %w", working, err)
	}
	// 一時的な名前で書いてから rename する。この直後に記録を捨てるので、ここで
	// 書きかけになるとアドレスは永久に失われ、切り詰められた写しだけが返せるものと
	// して残る。
	return writeAtomic(path, original)
}

// unclaimRestore は、確保した記録を「復元待ち」を意味する名前へ戻します。
func unclaimRestore(working string) error {
	record := strings.TrimSuffix(working, RestoringSuffix)
	if err := os.Rename(working, record); err != nil {
		return fmt.Errorf("papertracker: put %s back to %s: %w", working, record, err)
	}
	return nil
}

// claimRestore は、ブリッジ以前の記録を確保し、それをどう適用するかを返します。
// 読み出すファイルと、それが「キャッシュは無かった」を意味するかどうかです。
//
// 先に脇へ move することが、同じ復元が二度起きるのを防いでいます。作業名のもとに
// 既にある記録を決して適用しないのはそのためです。そうした記録が生まれる最もあり
// がちな経緯は、アドレスを戻したうえで自分のファイルを削除できなかった復元であり、
// 次の起動でそれをもう一度適用すると、その間にクライアントがキャッシュしたアドレス —
// ブリッジを使うのをやめた後にユーザーが選んだカメラ — を取り消してしまいます。
//
// その場合は、クライアントが記録の述べるものを既に持っていることで見分けがつき、
// 黙って片付けます。それ以外は復元が終わる前に止まったということで、推測せず報告
// します。アドレスはまだディスク上にあってユーザーが判断できますが、先へ進んだ
// キャッシュの上に書いてしまえば取り消せません。
func claimRestore(path string) (working string, erase bool, err error) {
	// 印の方が優先する。クライアントにキャッシュがまったく無かったと述べており、
	// バックアップがそれと並んで存在することはあり得ない。
	for _, record := range []struct {
		name  string
		erase bool
	}{
		{path + NoOriginalSuffix, true},
		{path + BackupSuffix, false},
	} {
		// 適用済みと述べている記録はゴミでしかなく、それを片付けるのがこの経路。
		switch dropped, err := dropApplied(record.name); {
		case err != nil:
			return "", false, err
		case dropped:
			return "", false, fmt.Errorf("%w at %s", ErrNoBackup, record.name)
		}

		claimed := record.name + RestoringSuffix
		switch found, err := exists(claimed); {
		case err != nil:
			return "", false, err
		case found:
			return "", false, tidyClaimed(path, claimed, record)
		}

		switch found, err := exists(record.name); {
		case err != nil:
			return "", false, err
		case !found:
			continue
		}
		if err := os.Rename(record.name, claimed); err != nil {
			return "", false, fmt.Errorf("papertracker: rename %s: %w", record.name, err)
		}
		return claimed, record.erase, nil
	}
	return "", false, fmt.Errorf("%w at %s", ErrNoBackup, path+BackupSuffix)
}

// tidyClaimed は、作業名のもとに残された記録を処理し、必ず何が起きたかを述べる
// エラーを返します。復元するものがもう無いか、残ったものに人の判断が要るかの
// どちらかです。
func tidyClaimed(path, claimed string, record struct {
	name  string
	erase bool
}) error {
	done, err := restoreLooksDone(path, claimed, record.erase)
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("%w: %s holds what the client had before PTCamBridge; rename it to %s to have it put back",
			ErrRestoreInterrupted, claimed, record.name)
	}
	if err := os.Remove(claimed); err != nil {
		return fmt.Errorf("papertracker: remove %s: %w", claimed, err)
	}
	return fmt.Errorf("%w at %s", ErrNoBackup, record.name)
}

// restoreLooksDone は、クライアントが既に記録の述べる状態にあるかを返します。
// 後片付けだけに失敗した復元と、そもそも完了しなかった復元を見分けるのがこれです。
func restoreLooksDone(path, claimed string, erase bool) (bool, error) {
	if erase {
		// 「ブリッジ以前にキャッシュは無い」という状態は、キャッシュが無いことに
		// よって復元される。
		found, err := exists(path)
		return !found, err
	}
	want, err := os.ReadFile(claimed)
	if err != nil {
		return false, fmt.Errorf("papertracker: read %s: %w", claimed, err)
	}
	got, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	return bytes.Equal(got, want), nil
}

// exists は path があるかどうかを返します。「見つからない」以外はすべて、否定の
// 答えではなく中断すべき理由として扱います。
func exists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("papertracker: stat %s: %w", path, err)
	}
	return false, nil
}

// recordNames は、ブリッジ以前の状態を表すファイルをすべて列挙します。進行中の
// 復元が使う名前も含みます。
func recordNames(path string) []string {
	return []string{
		path + BackupSuffix,
		path + BackupSuffix + RestoringSuffix,
		path + BackupSuffix + AppliedSuffix,
		path + NoOriginalSuffix,
		path + NoOriginalSuffix + RestoringSuffix,
		path + NoOriginalSuffix + AppliedSuffix,
	}
}

// ReadCache は、クライアントが現在キャッシュしているアドレスを返します。
func ReadCache(installDir string) (string, error) {
	data, err := os.ReadFile(CachePath(installDir))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// FindInstallDir は、よくある場所から PaperTracker のインストール先を探します。
// あくまで最善努力です。ユーザーはいつでも papertracker.install_dir を指定できます。
func FindInstallDir() (string, error) {
	for _, dir := range candidateDirs() {
		if dir == "" {
			continue
		}
		if looksLikeInstall(dir) {
			return dir, nil
		}
	}
	return "", ErrNotFound
}

// FindRestoreDir は、ブリッジが実際に書き込んだインストール先を探します。
//
// 復元に必要な答えは、書き込みのときとは違います。FindInstallDir は少しでも
// PaperTracker に見える最初のフォルダを返しますが、機械に複数ある場合 — 新しい
// コピーの隣にある古いコピー、Downloads にあるポータブル版 — それはブリッジが
// キャッシュを置き換えた相手ではない可能性がかなり高いのです。そこで復元しても
// バックアップは見つからず、「取り消すものは無い」と報告し、本当に変更された
// クライアントは、もう動いていないブリッジを指したまま残されます。
//
// 一致とみなすのはブリッジ自身のファイルだけです。バックアップにその名を付けるのと
// 同じ理由で、この探索はブリッジが一度も触れていないかもしれない機械でも走ります。
func FindRestoreDir() (string, error) {
	dirs, err := FindRestoreDirs()
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("%w in any of the usual PaperTracker folders", ErrNoBackup)
	}
	return dirs[0], nil
}

// FindRestoreDirs は、ブリッジが記録を残したフォルダをすべて列挙します。
//
// 複数あり得ます。write_cache が有効なまま install_dir が変わることは許されていて —
// クライアントが再インストールされたり移動されたり — 取り残されたフォルダには、
// ブリッジを指したままのクライアントと、以前の値を述べる記録が並んで残ります。
// 最初の 1 つだけを復元すると、そちらはそのまま残り、後から見直すものは何もありません。
func FindRestoreDirs() ([]string, error) {
	var dirs []string
	for _, dir := range candidateDirs() {
		if dir == "" || slices.Contains(dirs, dir) {
			continue
		}
		if hasBackup(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// WrittenDirsFile は、どのフォルダを自分へ向けたかについてのブリッジ自身の覚書です。
// クライアント側ではなく、ブリッジの設定と一緒に置かれます。
//
// 下の探索が知っているのはクライアントがよく置かれる場所だけですが、install_dir は
// まったく別の場所 — ユーザーが選んだフォルダにあるポータブルのコピー — を指定でき
// ます。その設定が消えたら、そして [papertracker] セクションを丸ごと削除するのが
// 既定へ戻る方法なのですが、そのフォルダの名前を挙げるものは二度と無くなります。
// 記録はクライアントの隣に残り、クライアントは止まったブリッジを指し続けます。
const WrittenDirsFile = "written-dirs.txt"

// RememberWrittenDir は、復元時に見るべき場所として installDir を書き留めます。
// 同じフォルダを二度記録しても何も起きません。
func RememberWrittenDir(stateDir, installDir string) error {
	installDir = strings.TrimSpace(installDir)
	if strings.TrimSpace(stateDir) == "" || installDir == "" {
		return nil
	}
	// 絶対パスで記録する。install_dir は相対でもよく、その場合それが指すフォルダは
	// ブリッジがどこから起動されたかに依存する。クライアント自身のフォルダからの
	// 手動実行と、次のサインインとでは別の場所になる。記録すべきは実際に書き込んだ
	// フォルダであって、後で別の意味になる言い回しではない。
	installDir, err := filepath.Abs(installDir)
	if err != nil {
		return fmt.Errorf("papertracker: resolve %s: %w", installDir, err)
	}
	known, err := WrittenDirs(stateDir)
	if err != nil {
		return err
	}
	if slices.Contains(known, installDir) {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("papertracker: create %s: %w", stateDir, err)
	}
	known = append(known, installDir)
	return writeAtomic(filepath.Join(stateDir, WrittenDirsFile), []byte(strings.Join(known, "\n")+"\n"))
}

// WrittenDirs は、RememberWrittenDir が記録したフォルダを列挙します。ファイルが
// 無いのは普通の答えです。ブリッジが一度もキャッシュを書いていないという意味です。
func WrittenDirs(stateDir string) ([]string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, nil
	}
	path := filepath.Join(stateDir, WrittenDirsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("papertracker: read %s: %w", path, err)
	}
	var dirs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !slices.Contains(dirs, line) {
			dirs = append(dirs, line)
		}
	}
	return dirs, nil
}

// hasBackup は、ブリッジが dir にブリッジ以前の状態を記録したかどうかを返します。
// それはクライアントのアドレスのバックアップか、無かったことを示す印のどちらかです。
// 途中まで進んだ復元も数に入ります。まだ終わらせる必要があるからです。
func hasBackup(dir string) bool {
	path := CachePath(dir)
	for _, name := range recordNames(path) {
		if _, err := os.Stat(name); err == nil {
			return true
		}
	}
	return false
}

// looksLikeInstall は、dir に PaperTracker と分かるもの — クライアントの実行
// ファイルか、それが書いたアドレスキャッシュ — があるかどうかを返します。
func looksLikeInstall(dir string) bool {
	for _, marker := range []string{"PaperTracker.exe", "paperTracker.exe", CacheFileName} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}
