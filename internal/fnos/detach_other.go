//go:build !unix

package fnos

import "syscall"

// detachSysProcAttr 在非 Unix 平台上是空操作。
//
// 重启自身这条路径只在飞牛（Linux）上跑得到：本机开发环境既没有
// appcenter-cli，Service.Available() 也会是 false，RestartSelf 会直接返回
// ErrCLIUnavailable。这里不人为造一个近似实现，免得制造"在 Windows 上
// 看起来能用"的错觉 —— 那比直接说不支持更危险。
func detachSysProcAttr() *syscall.SysProcAttr { return nil }
