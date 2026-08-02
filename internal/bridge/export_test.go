package bridge

// captureRunningForTest reports whether the capture goroutines are still
// alive, which is otherwise only visible under the lock.
func (b *Bridge) captureRunningForTest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.captureAsExpectedLocked()
}
