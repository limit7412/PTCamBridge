package bridge

import "github.com/limit7412/PTCamBridge/internal/config"

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
