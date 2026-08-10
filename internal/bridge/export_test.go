package bridge

import (
	"context"

	"github.com/limit7412/PTCamBridge/internal/config"
	"github.com/limit7412/PTCamBridge/internal/source"
)

// captureRunningForTest は、キャプチャの goroutine がまだ生きているかを返す。
// これは通常ロックの下でしか見えない。
func (b *Bridge) captureRunningForTest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.captureAsExpectedLocked()
}

// holdPendingForTest は、保存の失敗が残す状態にブリッジを置く。want はファイルまで
// 届かなかった設定、from はその書き込みが組み立てられた時点でファイルが持っていた
// 内容。
func (b *Bridge) holdPendingForTest(from, want config.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unsaved = &pendingSave{want: want, from: from}
}

// saveBaseForTest は、次の保存が土台にするもの。
func (b *Bridge) saveBaseForTest() (config.Config, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveBaseLocked()
}

// listingWaitersForTest は、走っている列挙の答えを待っている呼び出しの数を返す。
// 通常はロックの下にしか無い。
func (b *Bridge) listingWaitersForTest() int {
	b.modesMu.Lock()
	defer b.modesMu.Unlock()
	waiting := 0
	for _, call := range b.listing {
		waiting += call.waiting
	}
	return waiting
}

// scanWaitersForTest は、走っているカメラの列挙に相乗りした呼び出しの数を返す。
// 通常はロックの下にしか無い。
func (b *Bridge) scanWaitersForTest() int {
	b.modesMu.Lock()
	defer b.modesMu.Unlock()
	if b.scan == nil {
		return 0
	}
	return b.scan.waiting
}

// openingCameraForTest は、ブリッジがそのカメラを開いている最中の状態に置く。
// 通常は launchLocked が設定し、返るときに下ろす。
func (b *Bridge) openingCameraForTest(device string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opening = device
	b.publishView()
}

// isOpeningForTest は、開いている最中と記録されているカメラを返す。
func (b *Bridge) isOpeningForTest() string {
	return b.view.Load().opening
}

// listModesOnceForTest は、CameraModes の手前の判定を通り越して、登録のところ
// だけを踏む。呼び出し側の判定と登録の間に割り込まれた状況を作るために要る。
func (b *Bridge) listModesOnceForTest(ctx context.Context, device string) ([]source.Mode, error) {
	return b.listModesOnce(ctx, device)
}
