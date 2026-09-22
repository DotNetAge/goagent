package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBashNormalExecute 正常命令执行与输出回传。
func TestBashNormalExecute(t *testing.T) {
	b := Bash{}
	out, err := b.Execute(context.Background(), []byte(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("输出应包含 hello，得到 %q", out)
	}
}

// TestBashOutputTruncated 超限输出被截断并标注，避免内存耗尽。
func TestBashOutputTruncated(t *testing.T) {
	b := Bash{Timeout: 30 * time.Second}
	out, err := b.Execute(context.Background(), []byte(`{"command":"head -c 1048576 /dev/zero | tr '\\0' 'a'"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > bashOutputLimit+200 {
		t.Fatalf("输出应被截断到上限附近，得到 %d 字节", len(out))
	}
	if !strings.Contains(out, "已截断") {
		t.Fatal("截断输出应带标注")
	}
}

// TestBashTimeoutKill 命令超时被终止并返回超时语义错误。
func TestBashTimeoutKill(t *testing.T) {
	b := Bash{Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := b.Execute(context.Background(), []byte(`{"command":"sleep 30"}`))
	if err == nil {
		t.Fatal("超时命令应返回错误")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("超时应约 300ms 内返回，实际 %v", time.Since(start))
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误应携带超时语义，得到 %v", err)
	}
}

// TestBashProcessTreeKilled 超时后子进程树被整组击杀，不留孤儿。
func TestBashProcessTreeKilled(t *testing.T) {
	b := Bash{Timeout: 500 * time.Millisecond}
	// 子进程睡 60 秒并把自己写进后台：若只杀 bash 不杀组，sleep 会驻留
	if _, err := b.Execute(context.Background(), []byte(`{"command":"sleep 60 & wait"}`)); err == nil {
		t.Fatal("超时命令应返回错误")
	}
	time.Sleep(200 * time.Millisecond)
	// 查找仍在运行的 sleep 60 进程
	out, err := Bash{}.Execute(context.Background(), []byte(`{"command":"pgrep -f 'sleep 60' | head -1"}`))
	if err != nil {
		t.Fatalf("pgrep 执行失败: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("子进程树应被整组击杀，仍发现存活进程: %q", out)
	}
}

// TestBashEmptyCommand 空命令直接拒绝。
func TestBashEmptyCommand(t *testing.T) {
	b := Bash{}
	if _, err := b.Execute(context.Background(), []byte(`{"command":"  "}`)); err == nil {
		t.Fatal("空命令应返回错误")
	}
}
