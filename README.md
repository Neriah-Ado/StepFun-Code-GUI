# StepFun Code-GUI

**v1.0.0**

Step Code 的**子代理编排可视化面板**。终端里跑的 `subagent` / `workflow` 编排，
在浏览器里以液态玻璃界面实时呈现。

> 内部包名为 `step-orchestra`；仓库与产品名称为 StepFun Code-GUI。

```
Step Code 运行时 ──▶ TypeScript 扩展 ──▶ Go 网关 ──▶ 浏览器面板
   (pi-tui)          (只做事件转发)      (拓扑重建)    (液态玻璃)
```

---

## 为什么是这个架构

Step Code 的 UI 层是 `pi-tui`，差分渲染终端字符，插件**没有**注入自定义图形组件的公开 API。
终端也无法渲染真实的模糊与折射。因此：

- **扩展层**只做一件事——把宿主事件规范化为 JSONL，写入 Go 子进程的 stdin。不含任何业务逻辑。
- **Go 网关**重建拓扑、维护状态、通过 SSE 推送。
- **浏览器**负责全部图形与动效。

这样分层的好处：宿主 API 变化时只影响 `extensions/` 一个目录；面板可以脱离 Step Code 独立开发调试。

---

## 快速开始

```bash
# 1. 构建 Go 网关（产物 ~10MB，自包含无运行时依赖）
cd gateway
go build -o ../bin/step-orchestra-gateway .      # Windows 加 .exe

# 2. 本地端到端验证（不需要装 Step Code）
node tools/mock-feed.mjs

# 想手动看面板：
node tools/mock-feed.mjs --serve
```

`mock-feed.mjs` 会回放一个完整场景（一次 workflow 并行扇出 + 一次 subagent 串行链），
然后自动拉取 `/api/snapshot` 做断言并打印结果。

---

## 安装到 Step Code

```bash
step install /absolute/path/to/step-orchestra
step list
```

之后在 Step Code 里执行 `/orchestra`，会打印面板地址：

```
[step-orchestra] panel http://127.0.0.1:47810/?t=<token>
```

打开即用。网关在扩展加载时自动拉起，随会话结束退出。

### 包结构

`package.json` 通过 `"pi": { "extensions": ["extensions/index.ts"] }` 声明入口
（对应官方 `with-deps` 示例的写法；**顶层** `extensions` 字段宿主不识别）。

`step install` 把来源写入 `~/.stepcode/config.toml`：

```toml
# StepCode configuration
packages = [ "E:\\Project\\...\\step-orchestra" ]
```

> 官方文档描述的是 `~/.stepcode/agent/settings.json`，但本机 0.1.1 实测写入的是
> `~/.stepcode/config.toml`。以 `step list` 的输出为准。

也可以不走安装，用 `step -e <入口路径>` 临时加载。

---

## 事件契约

扩展订阅以下宿主事件（`pi.on(event, handler)`），字段以 `extensions/types.ts` 为准。

| 宿主事件 | 用途 |
|---|---|
| `session_start` | 会话元信息（ID / cwd / 模型） |
| `agent_start` / `agent_end` / `agent_settled` | 轮次与整体结算 |
| `tool_call` | 节点创建；`subagent` / `workflow` 标记为编排节点并提取 mode |
| `tool_execution_update` | **编排进度的主数据源**。0.1.1 载荷在 `event.partialResult`，0.84.x 在 `event.details` —— 两者都解析 |
| `tool_result` | 节点终态、耗时、token 用量 |
| `message_start` / `message_update` / `message_end` | 对话流：取 `assistantMessageEvent.text_delta` 累积成流式气泡 |
| `session_shutdown` | 关闭网关 |

`message_update` 每 token 触发一次，只取其中 `assistantMessageEvent.type === "text_delta"` 的增量；
thinking 与 toolcall 增量不转发 —— 它们已经以工具节点的形式呈现。

### 上游健壮性

`extensions/progress.ts` 对每个计数字段维护一份**别名表**
（如 `running` ← `running / runningCount / active / activeCount`），
并在 `partialResult / progress / workflowProgress / snapshot / details` 等包装层下钻最多三层。
宿主重命名字段时会退化为**部分读取**，而不是整块面板空白。

本机 Step Code 为 **0.1.1**，与 GitHub main（0.84.x）存在实测差异：

| 项 | 0.1.1（本机实测） | 0.84.x（GitHub main） |
|---|---|---|
| 进度载荷字段 | `event.partialResult` | `event.details` |
| `subagent` 工具 | 由示例扩展提供，参数 `{agent\|tasks\|chain}` | 内置，另有 `workflow` 工具 |
| 安装配置落点 | `~/.stepcode/config.toml` | 文档称 `settings.json` |
| 包入口声明 | `"pi": { "extensions": [...] }` | — |

因此 `inferMode()` 从**参数形状**推断 fan-out 模式：有 `chain` 数组 → chain，
有 `tasks` 数组 → parallel，有 `agent` / `task` → single，不依赖显式的 `mode` 字段。

---

## 安全设计

| 项 | 措施 |
|---|---|
| 网络暴露 | 只绑 `127.0.0.1`，端口冲突时自动顺延（最多 12 次） |
| 鉴权 | 每次运行生成 24 字节随机 token，`/events` 与 `/api/snapshot` 均校验，常数时间比较 |
| 凭据泄露 | 参数经过 `redact.ts`：`key/token/secret/password/authorization/cookie` 等键整值替换为 `***` |
| 体积膨胀 | 单字符串截断 2048 字符，每层最多 24 个键，递归深度上限 4 |
| 注入 | 前端一律使用 `textContent` 写入宿主数据；`innerHTML` 仅用于本文件内声明的静态图标路径 |
| 数据落盘 | 不写任何文件；token 只在扩展内存中流转 |

---

## 开发

```
step-orchestra/
├── extensions/          # Step Code 扩展（TypeScript，ESM，Node ≥22）
│   ├── index.ts         #   入口：事件订阅与转发
│   ├── bridge.ts        #   Go 子进程生命周期 + 反向通道解析
│   ├── conversation.ts  #   对话镜像：历史回放 / 流式累积 / 发送
│   ├── profiles.ts      #   凭据应用：registerProvider 热切换
│   ├── progress.ts      #   WorkflowProgress 防御式解析
│   ├── redact.ts        #   脱敏与截断
│   └── types.ts         #   线协议
├── gateway/             # Go 网关（标准库，零第三方依赖）
│   ├── main.go          #   stdin 读取 + HTTP/SSE + REST 端点
│   ├── state.go         #   拓扑状态机
│   ├── conversation.go  #   对话日志（有界、按 id 索引）
│   ├── profiles.go      #   凭据存储（原子写、掩码投影）
│   └── hub.go           #   SSE 广播
├── web/                 # 浏览器面板（零构建，原生 HTML/CSS/JS）
│   ├── liquid-glass.css #   设计系统 + 视图栈 + 切换按钮 + 配置面板
│   ├── glass.js         #   指针高光 / 视差倾斜
│   ├── tree.js          #   编排树渲染（增量复用）
│   ├── chat.js          #   对话视图（气泡复用 + 贴底滚动）
│   ├── profiles.js      #   API 配置选择器
│   └── app.js           #   SSE 客户端 + 模式切换 + 详情面板
└── tools/
    ├── mock-feed.mjs    # 端到端验证（编排 + 对话 + 反向通道）
    └── test-progress.mjs# 宿主载荷解析回归
```

改前端的调试循环：`node tools/mock-feed.mjs --serve`，改文件后刷新浏览器即可，无需构建。

---

## 对话模式与视图切换

面板有两个视图：**编排**（工具与子代理拓扑）和**对话**（与 agent 的聊天）。
两者在同一个 grid 单元格内常驻挂载，切换只翻转可见性。

### 切换按钮

| 项 | 设计 |
|---|---|
| 位置 | topbar 右端、连接状态指示灯左侧。它是全局视图控件，不隶属任何一侧面板 |
| 文案 | 显示**目标模式**，即「点它去哪」：编排视图下显示「对话」，对话视图下显示「编排」 |
| 图标 | 随文案联动 —— 目标为对话时是气泡，目标为编排时是节点图。纯 CSS 按 `data-target` 切换，不做 DOM 替换 |
| 悬停 | 上浮 1px、描边转主题紫、玻璃高光增强（240ms） |
| 按下 | `scale(0.972)`，过渡压缩到 90ms |
| 焦点 | `:focus-visible` 2px 紫色外圈，键盘可达 |
| 过渡中 | `aria-busy="true"` + `pointer-events: none`，240ms 窗口内杜绝连点 |
| 禁用 | 事件流断开时不可切换（`opacity: .45`、`cursor: not-allowed`、无悬停反馈） |

### 为什么切换不卡、不闪、不跳

| 风险 | 处理 |
|---|---|
| 布局跳动 | 两视图共享同一 `grid-area: stage` 单元格重叠，容器尺寸恒定 |
| 重建闪烁 | 视图**永不卸载**，切换只改 `opacity` / `visibility` |
| 滚动丢失 | DOM 存活 → 编排树与对话的滚动位置原样保留 |
| 选中丢失 | 选中节点是 DOM 属性，不随切换变化 |
| 草稿丢失 | 输入框 DOM 存活，切换后内容与光标都还在 |
| 数据重载 | 切换**不发起任何请求**；SSE 持续写入两个视图 |
| 动画竞态 | 240ms `aria-busy` 窗口吞掉连点；`prefers-reduced-motion` 下过渡降为 0.01ms |

### 对话数据流

```
输入框 ──POST /api/send──▶ 网关 ──stdout 一行 JSON──▶ 扩展 ──pi.sendUserMessage()──▶ 宿主
宿主 ──message_start/update/end──▶ 扩展（累积 text_delta）──▶ 网关 ──SSE──▶ 气泡原地更新
```

这是系统里唯一的反向通道：stdin 负责上行事件，stdout 负责下行指令。

发送按钮的禁用条件是复合的：输入为空、agent 忙碌（`ctx.isIdle()` 为 false）、或事件流断开。
agent 忙碌时发送不会失败 —— 扩展自动降级为 `deliverAs: "followUp"` 排队等待。

消息气泡按 id 复用 DOM，流式更新只改文本节点，不重建列表；滚动采用「贴底」策略
（距底 < 72px 时自动跟随，否则显示「回到底部」按钮），避免打断用户向上翻阅。

## API 配置切换

在多个账户的 coding Plan 之间切换，无需重启。

### 配置存储

| 项 | 决定 |
|---|---|
| 位置 | `~/.stepcode/agent/step-orchestra/profiles.json`（由网关写入） |
| 权限 | 目录 `0700`、文件 `0600`；Windows 上为尽力而为，由 ACL 兜底 |
| 原子性 | 先写 `.tmp` 再 rename，崩溃不会截断原文件 |
| 格式 | `{ version, activeId, profiles: [{ id, name, provider, apiKey, baseUrl?, addedAt }] }` |
| 加密 | **不加密**。与 `.netrc`、`~/.aws/credentials` 同级做法：明文 + 文件权限 |
| 浏览器 | **永不接触明文**。列表接口只返回 `keyHint`（形如 `sk-m…7890`） |

选择「网关持有明文」而非浏览器 localStorage：localStorage 对同源脚本完全可读，
而面板本身就运行在 localhost 上 —— 把密钥放进浏览器等于交给任何一段注入脚本。

### 切换如何即时生效

Step Code 的凭据走 model registry。官方文档明确：`registerProvider` 在初始加载之后调用
**立即生效，无需 `/reload`**；且配置形式只覆盖传入的字段，模型目录保持不变。

```
点击切换 → POST /api/profiles/activate → 网关写 activeId
        → stdout 下发 action{apply_profile, profile}
        → 扩展 pi.registerProvider(provider, { apiKey })
        → 下一个请求即使用新凭据
```

只传 `apiKey`（和可选的 `baseUrl`），因此不会重置该 provider 的模型列表。

### 失效检测

不主动探测端点，而是**监听真实流量**：扩展订阅 `after_provider_response`，
收到 401/403 时广播 `profile_status`，面板把当前配置标红并给出原因。零额外请求、零额外计费。

表单侧的静态校验：名称与 provider 必填；新增时密钥必填；密钥短于 12 字符标记为「长度异常」。

### 交互设计

| 元素 | 行为 |
|---|---|
| 触发按钮 | topbar 右侧，位于 metrics 与「对话」切换键之间。状态点 + 账户名 + 掩码密钥 |
| 状态点 | 绿=可用 / 灰=缺密钥 / 琥珀=长度异常 / 红=被拒绝（带呼吸动画） |
| 展开 | 点击展开玻璃浮层，同步 `aria-expanded`，箭头翻转 |
| 关闭 | 点击外部、Esc，或再次点击 |
| 列表项 | 名称 +「当前」徽标 + provider / 掩码 / baseUrl + 状态描述；当前项不可重复点击 |
| 行内操作 | 编辑、删除（删除走原生 `confirm` 二次确认） |
| 悬停 / 按下 | 与模式切换键同一套：上浮 1px / `scale(0.978)` |
| 编辑 | 密钥留空 = 保留原值（服务端合并），浏览器无需回填自己从未拿到过的密钥 |

## Token 用量显示

对话视图底部、输入框上方的一行指标文本，形式参照
[`Neriah-Ado/stepfun-usage-monitor`](https://github.com/Neriah-Ado/stepfun-usage-monitor)
的回复底部指标行。

```
537.3 tok/s · 首字 3.0s · 输出 223 tok / 生成 0.4s · 均值 494.9 · 累计 51.3k tok · 上下文 168k/200k · 接近上限 · 10:23:04
```

| 项 | 设计 |
|---|---|
| 形态 | **单行纯文本**、等宽字体、中点分隔 —— 无进度条、无独立面板背景，不构成第三个「surface」 |
| 位置 | 对话日志与 composer 之间，属于输入区 |
| 时机 | `message_end` —— 数值只在响应落地时变化，不轮询、不随 token 抖动 |
| 数字规则 | **双轨制**：精确值千分位（`2,762`）；累计值紧凑 —— <1k 原始、1k–10k 一位小数（`9.8k`）、10k–1M 取整（`51k`）、≥1M 一位小数 M（`73.8M`） |
| 时刻 | 右对齐，等宽，不参与文本截断 |
| ≥75% / ≥92% | 整行转琥珀 / 红，并追加「接近上限」/「即将溢出，建议 /compact」 |

### 速率是怎么算出来的

扩展层只有事件到达时间戳这一个时钟，因此：

```
TTFT      = 首个 text_delta 到达 − message_start
生成耗时   = 最后一个 text_delta − 第一个 text_delta
tok/s     = 本轮输出 token ÷ 生成耗时 × 1000
```

两条刻意的保守处理：**生成窗口短于 200ms 或长于 1h 的样本不计入滑动平均**（是噪声，不是吞吐）；
**只有一个 delta 时 `generationMs` 报 `null` 而不是 0**，避免前端算出除以零的天文数字。
TTFT 因含 IPC 延迟而偏大，作为仪表读数可接受。

### 与前一轮「上下文占用」的关系

参照项目显示的是**速率**，而上一轮要求的上下文占用是**窗口余量** —— 两者语义不同，
所以这里做成融合：形式与数字规则照参照项目，字段以速率类为主，**同时保留上下文占用与告警**，
因为「接近上限」是这一行上唯一可行动的信号。

## 液态玻璃实现

| 层次 | 手段 |
|---|---|
| 折射底 | `backdrop-filter: blur(22px) saturate(175%)` |
| 玻璃体 | 142° 白色渐变 + 1px 高光描边 + 四向 `inset` 阴影模拟厚度 |
| 镜面高光 | `radial-gradient` 跟随 `--gx/--gy`，由 `glass.js` 写入 |
| 立体感 | 卡片跟随 `--rx/--ry` 做 ≤4.5° 视差倾斜 |
| 液态形变 | 空状态光环应用 SVG `feTurbulence` + `feDisplacementMap` |
| 性能护栏 | rAF 合并指针事件；节点入场动画用 `backwards` 而非 `both`（否则会锁死 `transform` 导致 hover 失效）；`contain: layout paint` |
| 降级 | `@supports not (backdrop-filter)` → 高不透明纯色面板；`prefers-reduced-motion` → 关闭全部动效 |

> 说明：真实折射需要扭曲「透过玻璃看到的背景」，CSS 无法对 `backdrop` 施加位移滤镜。
> 因此折射感由高光、内阴影与倾斜共同营造；SVG 位移滤镜只用在装饰性元素上，避免文字扭曲。

---

## 验证状态

| 项 | 状态 |
|---|---|
| 扩展被 Step Code 加载并执行 | ✅ 实测（`step -e` 与自动发现两条路径） |
| 自动发现（`step install` → `config.toml`） | ✅ 实测（`step list` 确认） |
| 扩展生命周期钩子 | ✅ 实测（`session_shutdown` 后网关优雅退出，无残留进程） |
| 端口冲突自动避让 | ✅ 实测（47810 被占时顺延到 47811） |
| 网关 ↔ 前端数据链路（编排 + 对话 + 反向通道 + 凭据 + 用量） | ✅ `tools/mock-feed.mjs` **33/33** 断言 |
| 宿主载荷解析 | ✅ `tools/test-progress.mjs` 11/11 断言 |
| 凭据持久化落盘 | ✅ 实测 `profiles.json` 写入且含密钥 |
| 明文密钥不外泄到浏览器 | ✅ 实测响应体中不含明文 |
| 配置浮层层叠关系 | ✅ `tools/check-layers.mjs` 11/11（已验证该脚本能捕获原缺陷） |
| 反向通道鉴权（无 token 拒绝） | ✅ 实测返回 401 |
| 真实 agent 事件流（tool_call / 消息流） | ⚠️ **未验证** —— 需登录后跑真实任务才会触发 |
| `registerProvider` 热切换实际生效 | ⚠️ **未验证** —— 需登录后观察下一个请求 |

### 层叠关系如何复核

```bash
node tools/check-layers.mjs      # 静态判定，11 项断言
```

判据是纯静态的：**后代元素永远无法越过自己所属的层叠上下文根**。
所以只要校验「`.topbar` 的 z-index 严格大于 `.stage`」这一个不变量，
就能保证嵌套在 `.topbar` 内的配置浮层不会被内容区遮挡。
这个脚本在修复过程中被反向验证过 —— 把 `z-index` 改回 `2` 会立刻降到 9/11 并返回非零退出码。

面板上手工确认：点击 topbar 的「配置」按钮，浮层应完整覆盖在编排树 / 对话区之上；
把窗口缩到 1080px 以下（`.stage` 切换为单列布局）后依然如此。

## 已知限制

- **真实 agent 事件流未验证。** 本机 `~/.stepcode/auth.json` 为空（未登录），
  且非 TTY 环境下 step 不建立真实会话，因此 `session_start` 与工具事件没有实际触发。
  登录后跑一次 `subagent` 调用即可补上这一环。
- **本机为 0.1.1，接口形状按实测校准。** 若升级到 0.84.x，`partialResult` 会变为
  `details`，`subagent` 可能变成内置工具并新增 `workflow` —— 解析层已同时兼容。
- **子代理明细是聚合的。** 子代理在独立进程中运行，主进程观测不到其内部工具调用。
  面板只能呈现宿主在进度快照里给出的 agent 行。
- **扇出只有一层。** 这是 Step Code 的约束，因此树最深两层。
- **Windows 构建需带 `.exe`。** `bridge.ts` 已按平台自动选择文件名。
- **端口固定段 47810–47821。** 全部占用时会启动失败并在 `/orchestra` 中给出原因。

---

## 故障排查

| 现象 | 处理 |
|---|---|
| `/orchestra` 提示 binary missing | 未构建网关，执行 `cd gateway && go build -o ../bin/step-orchestra-gateway .` |
| 面板一直「连接中」 | 检查 URL 里的 `t` 参数是否完整；token 每次运行都会变 |
| 节点不出现 | 确认触发的是 `subagent` / `workflow`；普通工具调用需关闭「仅编排」筛选 |
| 端口被占 | `STEP_ORCHESTRA_PORT=50000` 环境变量覆盖起始端口 |
| 自定义网关路径 | `STEP_ORCHESTRA_BIN=/path/to/gateway` 环境变量覆盖 |
