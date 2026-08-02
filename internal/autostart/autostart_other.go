//go:build !windows

package autostart

const supported = false

func enabled(string) (bool, error) { return false, nil }
func enable(string) error          { return ErrUnsupported }
func disable() error               { return ErrUnsupported }
