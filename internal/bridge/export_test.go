package bridge

import "github.com/limit7412/PTCamBridge/internal/config"

// captureRunningForTest reports whether the capture goroutines are still
// alive, which is otherwise only visible under the lock.
func (b *Bridge) captureRunningForTest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.captureAsExpectedLocked()
}

// holdPendingForTest puts the bridge in the state a failed save leaves behind:
// want is the configuration that never reached the file, from is what the file
// held when that write was built.
func (b *Bridge) holdPendingForTest(from, want config.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unsaved = &pendingSave{want: want, from: from}
}

// saveBaseForTest is what the next save would build on.
func (b *Bridge) saveBaseForTest() (config.Config, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveBaseLocked()
}
