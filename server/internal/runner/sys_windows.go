//go:build windows

package runner

import (
	"os/exec"
	"strconv"
	"syscall"
)

// procAttr：Windows 下创建独立进程组，便于 taskkill /T 收整棵树。
// 0x00000200 即 CREATE_NEW_PROCESS_GROUP。
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200}
}

func terminate(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T").Run()
}

func forceKill(pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

func alive(pid int) bool {
	// Windows 没有 signal 0，用 tasklist 探测
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return false
	}
	return len(out) > 0 && !containsNoTask(out)
}

func containsNoTask(out []byte) bool {
	text := string(out)
	return text == "" || len(text) < 5
}
