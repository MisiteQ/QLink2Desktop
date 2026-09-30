package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic 以「同目录临时文件 + rename」的方式落盘。
//
// 这是相对早期版本的关键健壮性改进：早期直接 os.WriteFile 覆盖目标文件，
// 一旦写入过程中进程被杀 / 磁盘写满，就会留下一个半截的 JSON，
// 下次启动时整个链接列表静默丢失。rename 在同一文件系统内是原子的，
// 因此读取方永远只能看到「完整的旧版本」或「完整的新版本」。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()

	// 任何一步失败都要清理临时文件，避免目录里堆积垃圾。
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("原子替换 %s 失败: %w", path, err)
	}
	return nil
}

// readJSONFile 读取文件；文件不存在时返回 (nil, false, nil)，由调用方决定默认值。
func readJSONFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	return data, true, nil
}
