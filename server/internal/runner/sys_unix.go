//go:build !windows

package runner

import "syscall"

// procAttr 让 worker 进入独立进程组，停止时可以整组发信号。
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func terminate(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func forceKill(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func alive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil || syscall.Kill(pid, 0) == nil
}
