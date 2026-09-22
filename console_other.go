//go:build !windows

package main

// initConsole 在非 Windows 平台无需处理控制台编码。
func initConsole() {}
