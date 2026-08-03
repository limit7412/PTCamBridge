//go:build !windows

package tray

import "context"

// Run は ctx がキャンセルされるまでブロックします。
//
// トレイは Windows にしか存在しません。PaperTracker クライアントが配布されている
// のがそのプラットフォームだからです。それ以外ではブリッジはヘッドレスで動作し、
// 同じ操作面は管理 API から利用できます。
func Run(ctx context.Context, opts Options) {
	opts.Log.Info("no system tray on this platform, running headless", "address", opts.Address)
	<-ctx.Done()
	if opts.OnQuit != nil {
		opts.OnQuit()
	}
}
