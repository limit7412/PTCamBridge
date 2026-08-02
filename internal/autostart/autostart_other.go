//go:build !windows

package autostart

const supported = false

func enabled() (bool, error) { return false, nil }
func enable() error          { return ErrUnsupported }
func disable() error         { return ErrUnsupported }
