package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// runKey は、ユーザーごとの自動起動キーです。マシン全体の同等物と違い、HKCU は
// 昇格を必要としません。
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

const supported = true

// command は Run に書く値です。空白を含むパスでも起動できるよう実行ファイルの
// パスを引用符で囲み、設定ファイルが指定されていればそれを続けます。
//
// -config を持ち回すことが重要なのは、次のサインインが手にするのは登録された
// コマンドだけだからです。これが無いと、別の場所の設定ファイルで構成したブリッジが
// 既定のファイルで起き上がり、ユーザーが設定したのとは違うソースとポートで
// 黙って動くことになります。
func command(configPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("autostart: locate the executable: %w", err)
	}
	value := `"` + exe + `"`
	if configPath = strings.TrimSpace(configPath); configPath != "" {
		abs, err := filepath.Abs(configPath)
		if err != nil {
			return "", fmt.Errorf("autostart: resolve the settings path %q: %w", configPath, err)
		}
		value += ` -config "` + abs + `"`
	}
	return value, nil
}

func enabled(configPath string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("autostart: open the Run key: %w", err)
	}
	defer key.Close()

	value, _, err := key.GetStringValue(EntryName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("autostart: read the Run value: %w", err)
	}

	want, err := command(configPath)
	if err != nil {
		return false, err
	}
	// すでに移動した実体が残していったエントリは、有効なのではなく古いだけ。
	// 有効と報告すると、ユーザーはメニューからそれを直せなくなる。
	return strings.EqualFold(strings.TrimSpace(value), want), nil
}

func enable(configPath string) error {
	value, err := command(configPath)
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open the Run key: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue(EntryName, value); err != nil {
		return fmt.Errorf("autostart: write the Run value: %w", err)
	}
	return nil
}

func disable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("autostart: open the Run key: %w", err)
	}
	defer key.Close()

	if err := key.DeleteValue(EntryName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("autostart: delete the Run value: %w", err)
	}
	return nil
}
