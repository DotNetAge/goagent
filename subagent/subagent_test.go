package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── fake Dispatcher（goharness 第 2 步 Dispatcher 实现的验收规格载体）──

// fakeDispatcher 模拟宿主受理方：受理策略由 respond 注入，等待策略由 waitFn 注入。
type fakeDispatcher struct {
	respond func(req SubAgentRequest, n int) (SubAgentReceipt, error)
	waitFn  func(ctx context.Context, sessionIDs []string) map[string]error

	mu   sync.Mutex
	reqs []SubAgentRequest
}

func (d *fakeDispatcher) Submit(ctx context.Context, req SubAgentRequest) (SubAgentReceipt, error) {
	d.mu.Lock()
	n := len(d.reqs)
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	if d.respond != nil {
		return d.respond(req, n)
	}
	return SubAgentReceipt{Accepted: true, SessionID: fmt.Sprintf("session-%d", n)}, nil
}

func (d *fakeDispatcher) Wait(ctx context.Context, sessionIDs []string) map[string]error {
	if d.waitFn != nil {
		return d.waitFn(ctx, sessionIDs)
	}
	return map[string]error{}
}

// acceptArgs 构造合法的派发参数。
func acceptArgs(agentName, task string) json.RawMessage {
	args, _ := json.Marshal(SubAgentRequest{AgentName: agentName, Task: task})
	return args
}

// ── 并行 Submit 不坍缩 ──

// TestSubAgentParallelSubmitNoCollapse：N 个 goroutine 并行派发，
// 每个 Submit 都拿到唯一回执句柄、无请求丢失（工具层对并发完全透明）。
func TestSubAgentParallelSubmitNoCollapse(t *testing.T) {
	var seq atomic.Int32
	d := &fakeDispatcher{respond: func(req SubAgentRequest, n int) (SubAgentReceipt, error) {
		return SubAgentReceipt{Accepted: true, SessionID: fmt.Sprintf("s-%d", seq.Add(1))}, nil
	}}
	tool := NewSubAgentTool(d)

	const n = 50
	var wg sync.WaitGroup
	results := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := tool.Execute(context.Background(), acceptArgs("worker", fmt.Sprintf("task-%d", i)))
			if err != nil {
				out = fmt.Sprintf("ERROR: %v", err)
			}
			results <- out
		}(i)
	}
	wg.Wait()
	close(results)

	handles := make(map[string]bool, n)
	for out := range results {
		idx := strings.Index(out, "跟踪句柄：")
		if idx < 0 {
			t.Fatalf("回执文本应携带跟踪句柄，得到 %q", out)
		}
		handles[out[idx+len("跟踪句柄："):]] = true
	}
	if len(handles) != n {
		t.Fatalf("并行派发应产生 %d 个唯一句柄（不坍缩），得到 %d", n, len(handles))
	}
	d.mu.Lock()
	got := len(d.reqs)
	d.mu.Unlock()
	if got != n {
		t.Fatalf("Dispatcher 应收到 %d 个请求（无丢失），得到 %d", n, got)
	}
}

// ── 等待中 Cancel（Promise.all 语义 + ctx 短路）──

// TestSubAgentWaitCtxShortCircuit：Wait 阻塞等待期间父 ctx 取消，
// 等待必须被短路返回（宿主实现等待原语的规格演示）。
func TestSubAgentWaitCtxShortCircuit(t *testing.T) {
	d := &fakeDispatcher{waitFn: func(ctx context.Context, sessionIDs []string) map[string]error {
		<-ctx.Done()
		return map[string]error{}
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan map[string]error, 1)
	go func() { done <- d.Wait(ctx, []string{"s1", "s2"}) }()

	// 确保已进入等待
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("等待期间 ctx 取消应短路返回")
	}
}

// ── 重复 Submit ──

// TestSubAgentDuplicateSubmit：同一请求重复派发，工具层透明传递不报错、
// 每次都拿到独立句柄——去重/复用策略是宿主 Dispatcher 的职责，不在工具层。
func TestSubAgentDuplicateSubmit(t *testing.T) {
	var seq atomic.Int32
	d := &fakeDispatcher{respond: func(req SubAgentRequest, n int) (SubAgentReceipt, error) {
		return SubAgentReceipt{Accepted: true, SessionID: fmt.Sprintf("s-%d", seq.Add(1))}, nil
	}}
	tool := NewSubAgentTool(d)

	args := acceptArgs("worker", "same-task")
	out1, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if out1 == out2 {
		t.Fatalf("重复 Submit 应返回独立回执，两次一致: %q", out1)
	}
	d.mu.Lock()
	got := len(d.reqs)
	d.mu.Unlock()
	if got != 2 {
		t.Fatalf("Dispatcher 应收到 2 个请求，得到 %d", got)
	}
}

// ── Submit 被宿主拒绝 ──

// TestSubAgentSubmitRejected：宿主策略性拒绝（Accepted=false + Reason），
// 模型同步收到拒绝原因以便自纠。
func TestSubAgentSubmitRejected(t *testing.T) {
	d := &fakeDispatcher{respond: func(req SubAgentRequest, n int) (SubAgentReceipt, error) {
		return SubAgentReceipt{Accepted: false, Reason: "并发上限已满"}, nil
	}}
	tool := NewSubAgentTool(d)

	out, err := tool.Execute(context.Background(), acceptArgs("worker", "task"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "并发上限已满") {
		t.Fatalf("拒绝原因应回传给模型，得到 %q", out)
	}
}

// TestSubAgentSubmitError：受理过程失败（error 非拒绝）同步反馈给模型。
func TestSubAgentSubmitError(t *testing.T) {
	d := &fakeDispatcher{respond: func(req SubAgentRequest, n int) (SubAgentReceipt, error) {
		return SubAgentReceipt{}, fmt.Errorf("派发通道已关闭")
	}}
	tool := NewSubAgentTool(d)

	out, err := tool.Execute(context.Background(), acceptArgs("worker", "task"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "派发失败") || !strings.Contains(out, "派发通道已关闭") {
		t.Fatalf("受理失败应同步反馈给模型，得到 %q", out)
	}
}

// ── 参数校验（同步纠错反馈）──

// TestSubAgentParamValidation：必填参数缺失时同步拒绝，不打扰 Dispatcher。
func TestSubAgentParamValidation(t *testing.T) {
	d := &fakeDispatcher{}
	tool := NewSubAgentTool(d)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "agent_name 不能为空") {
		t.Fatalf("缺少 agent_name 应同步拒绝，得到 %q", out)
	}

	out, err = tool.Execute(context.Background(), json.RawMessage(`{"agent_name":"w"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "task 不能为空") {
		t.Fatalf("缺少 task 应同步拒绝，得到 %q", out)
	}

	d.mu.Lock()
	got := len(d.reqs)
	d.mu.Unlock()
	if got != 0 {
		t.Fatalf("校验失败不应到达 Dispatcher，实际收到 %d", got)
	}
}
