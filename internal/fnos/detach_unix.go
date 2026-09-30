//go:build unix

package fnos

import "syscall"

// detachSysProcAttr 让子进程另立会话（setsid）。
//
// 这一行是「重启自身」能成立的**唯一**支点，值得单独解释清楚：
//
// 飞牛应用中心只提供 start / stop，没有 restart（`appcenter-cli --help` 确认过）。
// 而 `stop` 是同步的 —— 它走应用的 cmd/main stop，后者向本进程发 TERM，
// 然后每秒轮询等它退出。也就是说：Stop 返回之前，本进程已经不存在了，
// "Stop 之后再 Start" 的那几行代码永远执行不到。
// 真机日志把这条钉死了：
//
//	[main] Stopping qlink2desktop...
//	[main] waiting process terminate... (1s/10s) ... (10s/10s)
//	[main] send KILL signal to PID:516508...
//	[main] qlink2desktop stopped.        ← 到此为止，应用再没起来
//
// 所以"等一会儿再把它拉起来"这件事只能交给一个**不属于本进程**的 shell。
// setsid 让它脱离会话：cmd/main 的 stop 只 kill PID 文件里记录的那一个 PID
// （见 stop_process 的实现），不会波及另立会话的子进程。
//
// 非 Unix 平台（开发机）返回 nil：那条路径只在真机上跑，本机没有 appcenter-cli。
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
