# Apex Pro 剥皮头（Scalping）自动化交易系统

> **语言**：Go 1.21+  
> **交易所**：[Apex Pro](https://pro.apex.exchange)（永续合约，USDC 保证金）  
> **最后更新**：2026-02-23

---

## 目录

1. [系统概述](#1-系统概述)
2. [项目结构](#2-项目结构)
3. [策略原理](#3-策略原理)
4. [运行逻辑](#4-运行逻辑)
5. [配置说明](#5-配置说明)
6. [风控说明](#6-风控说明)
7. [快速开始](#7-快速开始)
8. [日志说明](#8-日志说明)
9. [注意事项](#9-注意事项)

---

## 1. 系统概述

本系统是针对 **Apex Pro 永续合约**的剥皮头（Scalping）自动化做市策略。核心思路是在买一/卖一价附近同时挂限价买单和限价卖单，通过高频赚取买卖价差（Spread），并配合完整的风控模块保护账户安全。

**核心特性：**

- 通过 **WebSocket** 实时订阅订单簿，获取最优买一/卖一价
- 所有挂单使用 `POST_ONLY` 模式，确保只做 **Maker**，享受更低手续费
- 每 500ms 刷新一次挂单，保持报价始终贴近市场
- 独立的 **风控控制器**，支持单日亏损熔断、连续亏损熔断、余额保护、止盈止损
- 收到 `SIGINT` / `SIGTERM` 信号后**优雅退出**，自动撤销所有挂单

---

## 2. 项目结构

```
scalping/
├── main.go                 # 程序入口，信号监听与优雅退出
├── config.yaml             # 策略与风控配置文件
├── go.mod                  # Go 模块依赖
├── config/
│   └── config.go           # 配置结构体定义与 YAML 加载
├── apex/
│   ├── client.go           # Apex Pro REST API 客户端（签名、下单、撤单、持仓）
│   └── ws.go               # Apex Pro WebSocket 客户端（订单簿订阅）
├── strategy/
│   └── engine.go           # 剥皮头策略引擎（主循环、报价计算、挂单管理）
└── risk/
    └── controller.go       # 风控控制器（盈亏追踪、熔断、止盈止损）
```

### 模块职责

| 模块 | 文件 | 职责 |
|------|------|------|
| 入口 | `main.go` | 加载配置、初始化引擎、监听退出信号 |
| 配置 | `config/config.go` | 定义所有配置结构体，支持 YAML 文件 + 环境变量覆盖 |
| REST 客户端 | `apex/client.go` | HMAC-SHA256 签名、账户查询、下单、撤单、持仓查询 |
| WS 客户端 | `apex/ws.go` | WebSocket 连接管理、订单簿订阅、心跳保活（20s ping） |
| 策略引擎 | `strategy/engine.go` | 主循环（500ms）、盈亏监控循环（2s）、报价计算、挂单刷新 |
| 风控控制器 | `risk/controller.go` | 盈亏累计、连续亏损计数、熔断触发、止盈止损检查 |

---

## 3. 策略原理

### 3.1 剥皮头（Scalping）做市策略

剥皮头策略通过在买一价和卖一价之间同时挂单，赚取买卖价差：

```
市场订单簿：
  卖一价（Ask）: 50000.5
  买一价（Bid）: 50000.0
  价差（Spread）: 0.5 USDC

策略挂单（spread_ticks=1, tick_size=0.1）：
  买单价格 = Bid + 1×tick = 50000.0 + 0.1 = 50000.1
  卖单价格 = Ask - 1×tick = 50000.5 - 0.1 = 50000.4

理论单次盈利 = 50000.4 - 50000.1 = 0.3 USDC（扣除手续费前）
```

### 3.2 POST_ONLY 模式

所有订单使用 `TimeInForce: POST_ONLY`：
- 若挂单会立即成交（吃单），交易所会**自动拒绝**该订单
- 确保始终以 **Maker** 身份成交，享受更低（甚至负）手续费
- 避免因价格滑动导致意外吃单亏损

### 3.3 持仓控制

- 当净持仓绝对值 ≥ `max_position` 时，停止同向开仓
- 买单和卖单独立判断，不会因为一侧满仓而影响另一侧平仓

### 3.4 最小价差过滤

- 当市场买卖价差 < `min_spread_usdc` 时，跳过本轮挂单
- 防止在价差过小时挂单，避免手续费侵蚀利润

---

## 4. 运行逻辑

### 4.1 整体流程

```
程序启动
  │
  ├─ 加载 config.yaml（支持环境变量覆盖）
  ├─ 初始化 REST 客户端、WebSocket 客户端、风控控制器
  ├─ 建立 WebSocket 连接，订阅订单簿
  │    └─ WsClient 内部启动：reconnectLoop / readLoop / pingLoop
  │
  ├─ 启动 goroutine 1：主循环（mainLoop）
  │    └─ 每 500ms 执行一次 tick()
  │         └─ Step 0：检查 WS 是否已连接，断线中则跳过本轮
  │
  ├─ 启动 goroutine 2：盈亏监控循环（pnlMonitorLoop）
  │    └─ 每 2s 检查持仓变化，更新盈亏，检查止盈止损
  │
  ├─ 启动 goroutine 3：连接质量监控（wsQualityMonitor）
  │    └─ 每 30s 打印 WS 连接状态、RTT、重连次数、最近消息时间
  │
  └─ 等待 SIGINT / SIGTERM 信号
       └─ 收到信号 → Stop() → 撤销所有挂单 → 打印最终统计 → 退出
```

### 4.2 主循环 tick() 详细步骤

```
tick()
  │
  ├─ Step 0：WebSocket 连接检查
  │    └─ ws.Quality().Connected == false？→ 跳过本轮（等待重连）
  │
  ├─ Step 1：风控检查
  │    ├─ 查询账户可用余额
  │    └─ rc.Check(balance)
  │         ├─ 熔断状态？→ 拦截，撤单，跳过本轮
  │         ├─ 余额 < min_balance_usdc？→ 熔断
  │         ├─ 当日亏损 ≥ max_daily_loss_usdc？→ 熔断
  │         └─ 连续亏损 ≥ max_consecutive_loss？→ 熔断
  │
  ├─ Step 2：获取最优价（来自 WebSocket 缓存）
  │    └─ 价差 < min_spread_usdc？→ 跳过本轮
  │
  ├─ Step 3：计算挂单价格
  │    ├─ buyPrice  = round(Bid + spread_ticks × tick_size)
  │    └─ sellPrice = round(Ask - spread_ticks × tick_size)
  │         └─ buyPrice ≥ sellPrice？→ 价格异常，跳过
  │
  ├─ Step 4：查询当前净持仓
  │
  └─ Step 5：撤旧单 → 挂新单
       ├─ |pos| < max_position → 挂买单（POST_ONLY）
       └─ |pos| < max_position → 挂卖单（POST_ONLY）
```

### 4.3 盈亏监控循环（每 2 秒）

```
pnlMonitorLoop()
  │
  ├─ 查询当前持仓
  ├─ 对比上次持仓快照
  │    └─ 持仓减少 → 估算已实现盈亏 → rc.RecordTrade(pnl)
  │         ├─ 平多盈亏 = (当前 Bid - 开仓均价) × 平仓量
  │         └─ 平空盈亏 = (开仓均价 - 当前 Ask) × 平仓量
  │
  ├─ 更新持仓快照
  │
  └─ rc.CheckPnlTarget(take_profit, stop_loss)
       ├─ 累计盈亏 ≥ take_profit_usdc → 撤单 + 停止策略
       └─ 累计亏损 ≥ stop_loss_usdc  → 撤单 + 停止策略
```

### 4.4 WebSocket 心跳与重连机制

```
WsClient 后台 goroutine 架构：

  ┌─────────────────────────────────────────────────────┐
  │  reconnectLoop（常驻）                               │
  │    监听 reconnCh 信号 → 指数退避等待 → dial()        │
  │    重连成功 → resubscribeAll() 恢复所有订阅          │
  └──────────────────────┬──────────────────────────────┘
                         │ 每次 dial() 成功后启动
              ┌──────────┴──────────┐
              ▼                     ▼
        readLoop(conn)         pingLoop(conn)
        读取消息 → 分发回调    每 20s 发送带序号 Ping
        断线 → 写入 reconnCh  检测 Pong 超时 → 主动断线
```

**重连参数：**

| 参数 | 值 | 说明 |
|------|----|------|
| 初始退避 | 1s | 首次断线后等待 1 秒重连 |
| 最大退避 | 30s | 每次失败翻倍，上限 30 秒 |
| 连接超时 | 10s | `Dial` 握手超时 |
| Ping 间隔 | 20s | 每 20 秒发送一次 Ping |
| Pong 超时 | 10s | 超过 30s 未收到 Pong 则主动断线触发重连 |

**RTT 延迟检测：**
- 每次 Ping 携带递增序号，Pong 回传序号
- 通过 `sentAt` 与 `pongAt` 差值计算往返延迟（RTT）
- 每 30 秒由 `wsQualityMonitor` goroutine 打印连接质量报告

---

## 5. 配置说明

配置文件路径：`config.yaml`

### 5.1 API 配置

```yaml
api:
  base_url: "https://testnet.pro.apex.exchange"  # 测试网（上线前务必切换为主网）
  ws_url: "wss://pro.apex.exchange/realtime"
  api_key: ""        # Apex Pro API Key
  api_secret: ""     # Apex Pro API Secret
  passphrase: ""     # Apex Pro Passphrase
  l2_key: ""         # StarkEx L2 私钥（链上签名时使用）
```

> **安全提示**：生产环境建议通过环境变量传入密钥，不要将密钥写入配置文件。

**支持的环境变量（优先级高于配置文件）：**

| 环境变量 | 对应配置项 |
|----------|-----------|
| `APEX_API_KEY` | `api.api_key` |
| `APEX_API_SECRET` | `api.api_secret` |
| `APEX_PASSPHRASE` | `api.passphrase` |
| `APEX_L2_KEY` | `api.l2_key` |

### 5.2 策略参数

```yaml
symbol: "BTC-USDC"   # 交易对（Apex Pro 格式）

strategy:
  spread_ticks: 1          # 挂单偏移 tick 数（相对买一/卖一向内收紧）
  order_size: 0.001        # 单笔下单量（合约张数）
  max_position: 0.01       # 最大净持仓量（超过后停止同向开仓）
  refresh_interval_ms: 500 # 挂单刷新间隔（毫秒）
  take_profit_usdc: 50.0   # 累计止盈目标（USDC，达到后程序自动退出）
  stop_loss_usdc: 20.0     # 累计止损上限（USDC，超过后程序自动退出）
  min_spread_usdc: 0.5     # 最小价差阈值（低于此值不挂单）
  price_precision: 1       # 价格精度（小数位数，BTC 通常为 1）
  size_precision: 3        # 数量精度（小数位数）
```

| 参数 | 说明 | 建议值（BTC） |
|------|------|-------------|
| `spread_ticks` | 越小越激进，越大越保守 | 1~3 |
| `order_size` | 单笔量，影响每次盈亏绝对值 | 0.001 |
| `max_position` | 最大持仓敞口控制 | 0.01~0.05 |
| `refresh_interval_ms` | 刷新越快越贴近市场，但 API 调用越频繁 | 500~1000 |
| `min_spread_usdc` | 低于此值市场流动性差，不适合挂单 | 0.3~1.0 |

### 5.3 风控参数

```yaml
risk_control:
  max_daily_loss_usdc: 30.0    # 单日最大亏损（USDC）
  max_consecutive_loss: 5      # 最大连续亏损次数
  min_balance_usdc: 100.0      # 账户最低可用余额（USDC）
```

---

## 6. 风控说明

风控模块位于 `risk/controller.go`，独立于策略引擎，通过以下机制保护账户。

### 6.1 风控触发条件

| 触发条件 | 触发后行为 | 恢复方式 |
|----------|-----------|---------|
| 可用余额 < `min_balance_usdc` | 熔断，停止所有下单 | 充值后重启程序 |
| 当日亏损 ≥ `max_daily_loss_usdc` | 熔断，停止所有下单 | 次日自动重置（或重启程序） |
| 连续亏损次数 ≥ `max_consecutive_loss` | 熔断，停止所有下单 | 需人工重启程序 |
| 累计盈亏 ≥ `take_profit_usdc` | 撤单 + 程序自动退出 | 手动重启 |
| 累计亏损 ≥ `stop_loss_usdc` | 撤单 + 程序自动退出 | 手动重启 |

### 6.2 熔断 vs 退出的区别

- **熔断**：程序继续运行，但停止所有下单。每次 tick 都会打印熔断原因。适用于可自动恢复的场景（如次日重置当日亏损）。
- **退出**：程序撤单后直接退出进程。适用于达到盈利目标或触发总止损的场景。

### 6.3 盈亏计算说明

盈亏通过**持仓变化检测**估算：
- 每 2 秒对比当前持仓与上次快照
- 若持仓减少，说明有平仓成交，按 `(平仓价 - 开仓均价) × 平仓量` 估算已实现盈亏
- 此方式为**估算值**，与交易所实际结算可能存在微小差异（手续费未计入）

### 6.4 当日盈亏重置

风控控制器在每次调用时检查是否跨天，若跨天则自动重置 `dailyPnl`，`max_daily_loss_usdc` 限制每日独立生效。

---

## 7. 快速开始

### 7.1 环境要求

- Go 1.21 或以上
- 已在 Apex Pro 创建 API Key（需要交易权限）

### 7.2 安装依赖

```bash
cd scalping
go mod tidy
```

### 7.3 配置密钥

**方式一：环境变量（推荐）**

```bash
export APEX_API_KEY="your_api_key"
export APEX_API_SECRET="your_api_secret"
export APEX_PASSPHRASE="your_passphrase"
export APEX_L2_KEY="your_l2_key"
```

**方式二：直接写入 config.yaml**（不推荐用于生产）

```yaml
api:
  api_key: "your_api_key"
  api_secret: "your_api_secret"
  passphrase: "your_passphrase"
```

### 7.4 测试网验证

首次运行**务必使用测试网**，确认策略行为符合预期：

```yaml
# config.yaml
api:
  base_url: "https://testnet.pro.apex.exchange"
```

### 7.5 编译与运行

```bash
# 编译
go build -o scalping-bot .

# 运行
./scalping-bot

# 或直接运行
go run main.go
```

### 7.6 停止程序

```bash
# 发送 SIGINT（Ctrl+C）或 SIGTERM
kill -SIGTERM <pid>
```

程序收到信号后会：
1. 停止所有挂单刷新
2. 撤销当前所有挂单
3. 打印最终盈亏统计
4. 安全退出

---

## 8. 日志说明

程序运行时输出结构化日志，各前缀含义如下：

| 日志前缀 | 含义 |
|----------|------|
| `[策略]` | 策略引擎状态，挂单刷新信息 |
| `[下单]` | 新订单提交成功，含订单 ID、价格、数量 |
| `[撤单]` | 订单撤销结果 |
| `[盈亏]` | 检测到平仓成交，输出本次盈亏及累计统计 |
| `[统计]` | 每 2 秒输出一次持仓和盈亏快照 |
| `[风控]` | 风控事件，包括拦截下单、熔断触发、连续亏损计数 |
| `[WS]` | WebSocket 连接事件：连接成功、断线、重连、订阅恢复、Pong RTT |
| `[WS质量]` | 每 30 秒输出一次连接质量报告（RTT、重连次数、最近消息时间） |

**典型日志示例：**

```
[WS] 连接成功: wss://pro.apex.exchange/realtime
[WS] 已恢复订阅: orderbook.BTC-USDC
[策略] 已启动，交易对=BTC-USDC 价差=1 ticks 单笔=0.001
[下单] 买单 id=abc123 price=50000.1 size=0.001
[下单] 卖单 id=def456 price=50000.4 size=0.001
[策略] 刷新挂单 bid=50000.0 ask=50000.5 buy=50000.1 sell=50000.4 pos=0.0000 spread=0.5000
[WS] Pong 收到，RTT=12.34ms
[WS质量] 已连接 RTT=12.34ms 重连次数=0 最近消息=3秒前
[WS] 读取错误（将触发重连）: websocket: close 1006
[WS] 检测到断线，第 1 次重连，等待 1s ...
[策略] WebSocket 未连接（重连中），跳过本轮
[WS] 连接成功: wss://pro.apex.exchange/realtime
[WS] 已恢复订阅: orderbook.BTC-USDC
[盈亏] 成交平仓 size=0.0010 pnl=0.0003 USDC | 累计=0.0003 当日=0.0003 连续亏损=0
[统计] 持仓=0.0000 均价=0.0000 | 累计盈亏=0.0003 当日盈亏=0.0003 USDC
[风控] ⚠️  熔断触发！原因: 连续亏损 5 次超过限制 5 次
```

---

## 9. 注意事项

### ⚠️ 资金风险

- 剥皮头策略在**高波动行情**下可能连续亏损，务必合理设置 `max_daily_loss_usdc` 和 `stop_loss_usdc`
- 建议初始资金不超过总资产的 **10%** 用于测试
- **首次上线前必须在测试网完整验证策略行为**

### ⚠️ API 限频

- Apex Pro 对 REST API 有频率限制，`refresh_interval_ms` 建议不低于 **500ms**
- 每次 tick 会调用：`GetAccount`（1次）+ `GetPositions`（1次）+ 撤单（最多2次）+ 挂单（最多2次），共约 6 次 API 调用
- 若触发限频，程序会打印 HTTP 429 错误，可适当增大 `refresh_interval_ms`

### ⚠️ 网络稳定性

- WebSocket 支持**自动断线重连**，采用指数退避策略（1s → 2s → 4s ... 最大 30s）
- 重连成功后自动恢复所有订阅，策略无需重启
- 断线期间 `tick()` 会自动跳过，不会产生错误下单
- 建议在网络稳定的服务器上运行，减少重连频率

### ⚠️ 盈亏计算精度

- 当前盈亏为**估算值**，基于持仓变化检测，未计入手续费
- 实际盈亏以 Apex Pro 账户页面显示为准
- 手续费会侵蚀利润，需确保 `min_spread_usdc` 设置高于手续费成本

### ⚠️ 熔断后的处理

- 熔断触发后程序**不会自动退出**，会继续运行但停止下单
- 需人工检查原因后重启程序，或等待次日自动重置（仅限当日亏损熔断）
- 连续亏损熔断需人工重启，防止策略在异常行情中持续亏损

### ⚠️ 主网切换

上线主网前，务必修改 `config.yaml`：

```yaml
api:
  base_url: "https://pro.apex.exchange"   # 切换为主网地址
```

---

## 代码变更记录

| 日期 | 变更内容 |
|------|---------|
| 2026-02-23 | 初始版本：剥皮头策略引擎 + REST/WS 客户端 |
| 2026-02-23 | 新增独立风控模块（`risk/controller.go`），完善盈亏追踪、熔断、止盈止损逻辑 |
| 2026-02-23 | WebSocket 重构：新增断线自动重连（指数退避）、订阅自动恢复、连接质量监控（RTT 延迟检测、Pong 超时熔断）、`wsQualityMonitor` goroutine |
