// Package envx 提供统一的环境变量读取助手：去除首尾空白，缺省或非法时回退默认值。
package envx

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// String 返回去空白后的值；为空时返回 fallback。
func String(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// Int 解析整数；为空或非法时返回 fallback。
func Int(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// Duration 解析 time.Duration；为空、非法或非正数时返回 fallback。
func Duration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
