# goagent 安全与运维手册

本文覆盖安全最佳实践、部署指南与维护手册，面向把 goagent 作为内核接入的宿主开发者（goharness / daemon）。

## 一、安全模型

goagent 的信任边界只有一条：**LLM 生成的工具调用参数是不可信输入**。模型可能生成错误、危险或恶意的命令与参数，安全责任按层划分：

| 层 | 职责 |
| --- | --- |
| goagent 内核 | 循环协议完整性、控制平面状态机、挂起机制 |
| 工具实现（tools/ 及宿主自定义） | 参数校验、资源上限（超时/输出截断）、进程隔离 |
| 宿主（goharness / daemon） | 权限门控、并发上限、级联取消、审计日志 |

## 二、工具安全

### 内置 Bash 工具的防线

[tools/bash.go](../tools/bash.go) 内置三道防线：

1. **默认超时**：单条命令 2 分钟（`Bash.Timeout` 可覆盖，与 Context deadline 取更早者）；
2. **输出上限**：256 KB，超限截断并标注——超量输出既会撑爆内存，也会污染模型上下文；
3. **整树击杀**（unix）：命令运行在独立进程组，超时按组 SIGKILL，不留孤儿进程。

### 授权门控（生产必做）

Bash **不做命令白名单**——这是设计决定：白名单属于产品策略，不属于内核。生产环境的门控路径有两条：

- **挂起机制**：工具在执行前校验，不通过时返回 `ErrNeedExternalInput`（用 `NewPermissionRequest` 构造），循环挂起并发射 `EvSuspend`，由宿主路由给用户授权；
- **自定义 ToolExecutor**：通过 `WithToolExecutor` 注入带权限检查、审计日志的执行器，包一层 `DefaultToolExecutor` 即可。

### 工具输出进入模型上下文

工具执行结果会原样写进对话消息。敏感环境（生产凭据、内网拓扑）下应确保命令输出不包含秘密——必要时在自定义 executor 里做脱敏。`History` 注入的历史消息同样不经任何过滤，宿主自行保证来源可信。

## 三、多 Agent 安全（控制平面与派发）

- **登记表内部化（可观测性防线）**：控制平面是内核设施——登记表只有包内唯一的 `DefaultRuntimeManager()` 实例，所有运行自动登记，外部禁止创建实例。这杜绝了"自建登记表、运行不可观测"的监控盲区；
- **创建权上收宿主**：`SubAgent` 工具（独立子包 `goagent/subagent`，内核零感知）只能发请求，子 Runtime 由宿主 `SubAgentDispatcher` 受理创建——内核不可自我派生，杜绝失控的递归派生；
- **并发上限是宿主策略**：Dispatcher 在 `Submit` 里实现信号量/队列上限（如满载返回 `Accepted=false`），内核不限制；
- **级联取消**：宿主按 `RuntimeInfo.SponsorID` 过滤 `List()` 后逐个 `Cancel()`，内核不做级联；
- **runtimeID 生命周期**：同 ID 终态后不可复用（fail fast）；挂起恢复用新 runtimeID，跟踪句柄用会话 ID；
- **崩溃恢复**：登记表是内存态活跃视图，重启后为空——宿主扫描会话存储识别被中断的任务，这是特性不是缺陷。

## 四、配置与凭据

- API 密钥只经环境变量（`GOAGENT_API_KEY`）或 `WithAPIKey` 注入，**不落盘、不写进代码**；本仓库不含任何真实凭据（`DefaultAPIKey = "ollama"` 是 Ollama 的占位约定）；
- 密钥可能出现在进程环境与内存中，宿主进程的日志设施不得打印 `Config` 结构体；
- 仓库无 `.env`、无硬编码端点之外的地址。

## 五、事件总线契约

`InProcessEventBus` 的语义与约束（安全审计后定稿，详见 events.go 注释）：

- 订阅通道**无缓冲**，Emit 阻塞直到所有订阅者接收（同步可靠，不丢事件）；慢消费者会拖慢循环——高吞吐场景自行实现带缓冲的 EventBus；
- Emit 先快照订阅者并**释放总线锁**后投递：投递阻塞期间不持锁，订阅/取消/关闭不会被饿死；
- `Subscribe` 返回的取消函数**幂等且永不阻塞**：仅置位（墓碑），已取消的订阅者在投递时被跳过；
- 取消**不关闭通道**：所有订阅者通道（含已取消的）由 `Close()` 统一关闭，消费侧以通道关闭为终止信号（`for range ch` 可安全使用）；
- `Close` 必须由发送方生命周期收尾调用（内核 `defer bus.Close()` 天然满足）：此刻已无在途投递，不存在向已关闭通道发送；`Close` 后 Emit 变空操作；
- 每次运行结束内核会关闭注入的 bus：**同一 bus 实例跨多次运行复用时事件会静默丢失**，跨运行复用需宿主自行管理（如每次运行注入新实例）。

## 六、部署指南

### 依赖与工具链

- Go ≥ 1.25（`go.mod` 的 `go` 指令）；标准库安全修复依赖编译环境工具链版本，建议跟踪最新 patch 版本；
- 唯一直接依赖 `github.com/DotNetAge/gochat`，升级流程：

```bash
go get -u ./...          # 升级到最新稳定版
go mod tidy
go list -m -versions github.com/DotNetAge/gochat   # 确认目标版本
```

- 漏洞扫描（发布前必跑）：

```bash
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
```

### 构建产物

- CLI：`go build -o goagent ./cmd/goagent`；
- 跨平台：tools 包已按 `//go:build unix` / `//go:build windows` 分离进程组击杀逻辑，windows 下超时只杀直接子进程，属预期行为。

## 七、维护手册

### 测试

```bash
go test -race ./...      # 全量回归（-race 必开）
go test -race -run 'TestControlPlane' .      # 控制平面八条验收
go test -race ./tools/                       # Bash 工具安全回归
```

- `runtime_test.go` 的八条测试是控制平面的**验收规格**，改动状态机必须保证其全绿；
- 嵌套派发端到端验收在内核 `runtime_test.go`（独立登记表隔离），工具自身并发规格在 `subagent/subagent_test.go`——第 2 步 goharness 实现以此为准；
- `tools/bash_test.go` 覆盖超时、整树击杀、输出截断，改动 bash.go 必须全绿。

### 发布清单

1. `gofmt -l .` 为空；`go vet ./...` 通过；
2. `go test -race ./...` 全绿；
3. `govulncheck ./...` 无"代码可达"漏洞（标准库环境性漏洞须在报告中注明工具链要求）；
4. 敏感信息扫描：`grep -rniE 'sk-|password\s*=\s*"|secret\s*=\s*"' --include='*.go' .` 无命中；
5. README 与 DESIGN.md 与代码同步（Option 表、事件表、目录结构、架构图）。

### 已知边界（v1 不做）

- 不做工具参数的 schema 强校验（由各工具自行解析）；
- 不做事件持久化与重放；
- 不做登记表持久化（崩溃恢复走会话层协议，见控制平面文档 §5.8）；
- 设计定稿见 [docs/PR-SubAgent-Control-Plane.md](PR-SubAgent-Control-Plane.md)。
