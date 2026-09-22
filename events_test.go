package goagent

import (
	"sync"
	"testing"
	"time"
)

// TestInProcessEventBusCancelIdempotent 取消函数幂等：二次调用不 panic（安全回归）。
func TestInProcessEventBusCancelIdempotent(t *testing.T) {
	bus := NewInProcessEventBus()
	_, cancel := bus.Subscribe()
	defer bus.Close()

	cancel()
	cancel() // 二次调用历史上会 close 已关闭通道导致 panic
	cancel()
}

// TestInProcessEventBusEmitSkipsCancelled 取消后的订阅者在投递时被跳过：
// Emit 不再阻塞等待已取消的消费者。
// 安全回归：修复 Emit 持总线锁阻塞投递 → Cancel 拿不到锁 → 整条总线死锁的问题。
func TestInProcessEventBusEmitSkipsCancelled(t *testing.T) {
	bus := NewInProcessEventBus()
	defer bus.Close()

	ch, cancel := bus.Subscribe()
	cancel()

	// 已取消 → 投递被跳过，Emit 必须立即返回（历史上会永久阻塞）
	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.Emit(Event{Type: EvStop, Data: StopFinished})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit 对已取消的订阅者仍然阻塞，取消未生效")
	}
	// 通道不因取消而关闭（由总线 Close 统一关闭），且不应收到任何事件
	select {
	case ev := <-ch:
		t.Fatalf("已取消的订阅者仍收到事件: %+v", ev)
	default:
	}
}

// TestInProcessEventBusConcurrentCancelEmit 并发取消与发布无竞态（-race 回归）。
// 每个订阅者配备消费者 goroutine，模拟真实用法：消费侧持续接收，随总线关闭退出。
func TestInProcessEventBusConcurrentCancelEmit(t *testing.T) {
	bus := NewInProcessEventBus()

	var actors sync.WaitGroup  // 取消者与发布者
	var readers sync.WaitGroup // 消费者（依赖总线关闭退出）
	for i := 0; i < 8; i++ {
		ch, cancel := bus.Subscribe()
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range ch {
			}
		}()
		actors.Add(1)
		go func() {
			defer actors.Done()
			time.Sleep(time.Millisecond)
			cancel()
		}()
	}
	for i := 0; i < 8; i++ {
		actors.Add(1)
		go func() {
			defer actors.Done()
			// 与取消并发：未取消的订阅者正常收到，已取消的跳过
			bus.Emit(Event{Type: EvStop, Data: StopFinished})
		}()
	}
	actors.Wait()
	bus.Close() // 统一关闭订阅者通道，释放消费者
	readers.Wait()
}

// TestInProcessEventBusEmitAfterClose Close 后 Emit 不得 panic。
func TestInProcessEventBusEmitAfterClose(t *testing.T) {
	bus := NewInProcessEventBus()
	bus.Subscribe()
	bus.Close()
	bus.Emit(Event{Type: EvStop, Data: StopFinished}) // 空操作，不 panic
}

// TestInProcessEventBusCloseReleasesConsumers 总线 Close 关闭所有订阅者通道，
// 消费者的 range 循环随之退出（终止信号契约回归）。
func TestInProcessEventBusCloseReleasesConsumers(t *testing.T) {
	bus := NewInProcessEventBus()

	var readers sync.WaitGroup
	received := make(chan int, 8)
	for i := 0; i < 4; i++ {
		ch, _ := bus.Subscribe()
		readers.Add(1)
		go func() {
			defer readers.Done()
			n := 0
			for range ch {
				n++
			}
			received <- n
		}()
	}

	bus.Emit(Event{Type: EvStop, Data: StopFinished}) // 4 个消费者各收 1 次
	bus.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		readers.Wait()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("总线 Close 后消费者未随通道关闭退出")
	}
	for i := 0; i < 4; i++ {
		if n := <-received; n != 1 {
			t.Fatalf("订阅者收到 %d 次事件，期望 1 次", n)
		}
	}
}
