package api

import (
	"fmt"
	"path"
	"strings"
)

// sanitizePath 对路径进行安全检查，防止路径遍历攻击。
// 拒绝空路径、绝对路径、包含 .. 的路径、以及以 . 开头的相对路径。
func sanitizePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(name, "/") || (len(name) >= 2 && name[1] == ':') {
		return "", fmt.Errorf("absolute path not allowed: %s", name)
	}
	if strings.Contains(name, "\\") {
		return "", fmt.Errorf("invalid path separator: %s", name)
	}
	if name == "." || strings.HasPrefix(name, "./") {
		return "", fmt.Errorf("invalid path: %s", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path traversal not allowed: %s", name)
	}
	return cleaned, nil
}
