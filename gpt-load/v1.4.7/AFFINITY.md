# Claude Affinity 设计

## 背景与目标

`gpt-load` 默认按轮询从 key 池分配上游 key（[keypool/provider.go:42](internal/keypool/provider.go#L42) 的 `store.Rotate`）。Claude prompt cache 按 **workspace 隔离**，跨账户 key 之间不互通；TTL 默认 5 分钟。轮询会让相同前缀的请求散落到不同 workspace，等于重复写 cache（1.25× 价格）且命中率为零。

**目标**：相同内容的请求绑定到上次成功的 key，最大化 cache 命中率；其他流量不受影响。

仅对 Anthropic Claude 实现，代码结构预留 OpenAI / Gemini。

## 只读保证

对请求体 / URL / Header / 参数**零修改**。复用 [server.go:102](internal/proxy/server.go#L102) 已经 `io.ReadAll` 出来的 `bodyBytes`，归一化全在 affinity 包内部局部变量上做，发往上游的字节与原始字节一致。输出只有 sha256 + key_id。

## 配置

```
CLAUDE_AFFINITY_ENABLED=false   # 总开关，默认关
CLAUDE_AFFINITY_TTL=3600        # 映射 TTL（秒），默认 1 小时
```

启动时读一次。关闭时行为与改动前完全一致。不动数据库 schema、前端 UI、Group 配置或 `types.ConfigManager` 接口。

## 资格门控

请求必须同时满足以下条件，否则跳过亲和：

| 条件 | 说明 |
|---|---|
| `group.ChannelType == "anthropic"` | 仅 Claude channel 注册了 Fingerprinter |
| `model` 匹配 `^claude-.*$` | |
| `c.Param("path") == "/v1/messages"` | gin 通配捕获的上游路径（不是 `c.Request.URL.Path`，那个还带 `/proxy/{group}` 前缀）|
| body 是合法 JSON | 由 `hasCacheControl` 内部 unmarshal 兼任检查 |
| **请求显式启用了 prompt cache** | 见下方"为什么要求 cache_control" |
| `messages` 第一条 `role=user`，且 `first_user_text` 非空 | content 为非空字符串、或数组中至少含一个非空 `type=text` 块。见下方"为什么需要 first_user_text" |

### 为什么要求 cache_control

[`hasCacheControl`](internal/affinity/claude.go) 扫四个位置（顶层 / `system[]` / `tools[]` / `messages[].content[]`），任一处有非 null `cache_control` 即视为启用。

没启用 cache 的请求即便绑同一 key 也无收益，反而限制负载均衡——所以直接跳过。

这跟"指纹归一化剔除 cache_control"不冲突：**检测**用它判定是否启用 cache，**指纹**剔除它让加减断点不影响绑定。

### 为什么需要 first_user_text 非空

若空（messages 空 / 首条非 user / content 全是非 text 块如 image / document / tool_result / search_result）就跳过。否则 `system+tools` 相同的所有不同用户/会话会塌缩到同一 fp、错绑到同一 key，破坏负载均衡。这种情况只在"纯图像 / 纯文档 / 纯工具回合首请求"出现，比例极低，可接受不亲和。

## 指纹算法

```
sha256( "strict" | model | normalized_system | canonical_tools | first_user_text )
```

KV 之间用 `\x00` 分键值、`\x01` 分项，避免拼接歧义。

### 各字段归一化

| 字段 | 处理 |
|---|---|
| `model` | 直接用 `channelHandler.ExtractModel(c, bodyBytes)` 提取的字符串 |
| `system` | string 直接用；array 只取 `type=text` 块 `\n` 拼接；**剔除 cache_control**；**剔除 `text` 以 `x-anthropic-billing-header` 开头的块**（见下"为什么剔除 billing header 块"）|
| `tools` | 每个 tool 取 `(name, description, type)` 三元组；**按 (name, type, description) 升序**（避免数组顺序影响，三级 tiebreak 防 `sort.Slice` 不稳定）；`\x1f` 分字段 `\x1e` 分工具；**剔除 cache_control**；**不抓 `input_schema`**（见下"为什么不抓 input_schema"）|
| `first_user_text` | 第一条 `role=user` 的 content：string 直接用；array 只取 `type=text` 块 `\n` 拼接 |

### 为什么剔除 billing header 块

Claude Code 等客户端会在 system 数组里注入一个独立的 text 块，形如：

```json
{
  "system": [
    {
      "type": "text",
      "text": "x-anthropic-billing-header: cc_version=2.1.145.61f; cc_entrypoint=claude-vscode; cch=adbaf;"
    },
    {
      "type": "text",
      "text": "(真正的 system prompt)",
      "cache_control": { "type": "ephemeral" }
    }
  ]
}
```

这块内容是给上游计费用的（cc_version 是客户端版本、cch 是会话/请求级别的随机串等），**Anthropic 服务端计算 prompt cache 时本身就不会把它纳入 cache key**——所以同一会话内两个相邻请求即便这一行 payload 不同，服务端 cache 仍然命中。

我们的亲和指纹需要跟服务端 cache 行为对齐：如果把这个块喂进 sha256，cc_version / cch 一变指纹就漂，相同会话的请求会算出不同 fp、无法绑到同一 key，亲和直接失效。所以在 [`joinSystemTextBlocks`](internal/affinity/claude.go) 里按 `strings.HasPrefix(text, "x-anthropic-billing-header")` 判定后跳过。

范围严格限定在 **system 数组**：字符串型 system、`tools`、`messages[].content` 都不过滤。原因——观察到的注入点就在 system 数组，越窄越不会误伤正文里偶尔出现这串字符的情况。由 `TestClaude_Fingerprint_StripsBillingHeaderInSystem` / `TestClaude_Fingerprint_BillingHeaderStripIsPrefixOnly` 锁住。

### 为什么不抓 input_schema

`canonicalTools` 只抓 `(name, description, type)`，**跳过 `input_schema`**。原因：input_schema 是嵌套 JSON 对象，字节级表示依赖客户端的 JSON 序列化器——字段顺序、空格、转义、`null` 字段省略与否，不同客户端甚至同一客户端的不同版本都可能产生**语义等价但字节不等**的 schema。如果把它的 raw bytes 喂进哈希，会让本该归到同一会话的请求算出不同指纹，反而打击亲和。

代价：仅 input_schema 不同而 `(name, description, type)` 全相同的两个工具会撞到同一 fp。实际很少见——schema 改动通常伴随 name/description 改动。即使真撞了：亲和层只决定路由到哪个 key，**上游 cache 命中与否由 Anthropic 按完整字节判定**，最坏是一次 cache miss + write，绝不会因此请求失败。指导原则：**把"差不多的请求"路由到同一个 key，是否真命中 cache 让 Anthropic 来判**。

`type` 保留是为了区分 server tools 的版本——如 `{"type":"computer_20241022","name":"computer"}` vs `{"type":"computer_20250124","name":"computer"}`，name 相同但是不同上游工具，cache 不互通必须分开绑。client tools（Claude Code 流量主体）没有 `type` 字段，此处归一到空串，等价于只看 `(name, description)`。由 `TestClaude_Fingerprint_ServerToolVersionDiffers` / `TestClaude_Fingerprint_IgnoresInputSchema` 锁住。

### system / tools 为空都能算指纹

字段缺失 / `null` / `""` / `[]` 四种"空"表达会归一到同一空字符串，只要 `first_user_text` 非空就照常算指纹。覆盖"简单聊天没有 system / tools"场景。tool 内单字段（name / description / type）同样支持 缺失 / `null` / `""` / 非字符串四种"空"形态归一。由 `TestClaude_Fingerprint_EmptySystemAndToolsRobustness` 和 `TestClaude_Fingerprint_ToolFieldEmptyForms` 锁住。

### 与 Claude 服务端 cache 行为的对齐

| 失效维度 | 我们指纹 | 对齐 |
|---|---|---|
| Tools 改动（name / description / type / 增删工具）| 重算 | ✅ |
| Server tool 版本（computer_20241022 → computer_20250124）| 重算 | ✅ |
| Web search / Citations toggle（本质是 tools 数组改动）| 重算 | ✅ |
| System 改动 | 重算 | ✅ |
| 第一条 user 改动 | 重算 | ✅ |
| `x-anthropic-billing-header` 块变化（cc_version / cch 等）| 不变 | ✅（服务端 cache 也不算入）|
| 仅 `input_schema` 改动（name/description/type 不变）| 不变 | ⚠️ 设计取舍（见"为什么不抓 input_schema"）|
| 顶层 `speed` / `tool_choice` / `thinking` / `output_config` / images 等运行时参数 | 不变 | ⚠️ 设计取舍 |

不一致的几项都是会话内基本固定的运行时参数。即使切换也只是"一次 cache miss + write"，不影响正确性。换来的是更高的总体命中率。

## 存储

复用 [store/](internal/store/) 抽象，Redis / MemoryStore 都已支持 `SetNX` / `Get` / `Delete`。

```
key:   gpt-load:affinity:v1:{group_id}:{fingerprint}
value: {key_id}（字符串数字）
ttl:   CLAUDE_AFFINITY_TTL
op:    SET NX EX（先到先得）
```

为什么 SETNX 而非 SET：避免并发首次请求的"最后写赢"抖动；命中场景下 Record 自动 no-op。

## 选 key 与重试

接入点 [proxy/server.go:127](internal/proxy/server.go#L127) `executeRequestWithRetry`，新增 `affinityFP / affinityHit` 两个透传参数。

```
首次 (retryCount == 0):
  tryAffinityKey → 计算 fp + 查 Lookup
    命中且 key active → 使用该 key（hit）
    命中但 key 失效 → Delete + 回退轮询（unbind）
    未命中 → 回退轮询（miss）
    不满足资格 → 透传 fp="" 跳过（skip）

重试 (retryCount > 0):
  跳过亲和分支，正常轮询；affinityFP / affinityHit 透传给递归调用

响应处理:
  失败 + affinityHit==true → 立即 Delete 旧映射，affinityHit←false（不等 isLastAttempt）
  最终成功 + affinityFP!="" → Record（SETNX）
  最终失败：无额外处理（Delete 已在失败时做了）
  客户端中断（IsIgnorableError，连接断开/cancel）→ 直接 return，不动绑定（不是 key 失败）
```

### 三条不变式

1. **绑定的 key 必然是某个时刻成功过的 key**——失败 key 永远不会被绑定（命中失败立即 Delete，最终成功才 SETNX）
2. **同一 fp 最多一个有效绑定**——SETNX 防抖动，Delete 防残留
3. **请求只在启用 prompt cache 时才建立映射**——`hasCacheControl` 门控让无收益的请求不污染存储

### 为什么命中失败要立即 Delete

如果等 `isLastAttempt` 才 Delete，重试链救回来时 SETNX 会因旧映射存在而 no-op，下次同 fp 请求继续命中失败 key（直到该 key 失败计数达拉黑阈值）。立即 Delete 让重试成功后能正常绑到新 key。

### 为什么客户端中断不删绑定

`app_errors.IsIgnorableError` 覆盖连接断开 / context cancel 类错误——这些不是 key 的问题，是客户端走了。删掉绑定相当于让该 key 背锅，下次同会话反而拿不到 cache。所以这条路径直接 return，绑定保持原样。

### 为什么重试期间不查亲和

否则上一次失败的 key 还会被命中并再次选中。retryCount>0 时强制走轮询，确保重试拿到新 key。

## 日志可视化

`RequestLog.affinity_status` 反映该次 attempt 的亲和结果。状态是**每次 attempt 一条**：重试场景下首次 attempt 可能 `hit→unbind`，重试那条是空。

| 值 | UI | 含义 / 出现时机 |
|---|---|---|
| `""` | `-` | **与亲和无关**：非 Anthropic channel、亲和关闭、retry attempt |
| `skip` | 跳过 (info) | 属于亲和范围但本请求不满足资格——最常见是**客户端没用 cache_control**。看到 skip 通常意味客户端配置需要检查 |
| `miss` | 未命中 (default) | 满足条件但 Redis 没找到映射——新会话首请求 / TTL 过期 / 上次失败后。**正常状态**，成功后建立映射，下次同会话变 hit |
| `hit` | 命中 (success) | 用上了之前绑定的 key，很可能享受到 Claude prompt cache 折扣。亲和工作中的状态 |
| `unbind` | 解绑 (warning) | 映射在本 attempt 被主动删除——命中后失败 或 命中但 key 已失效 |

> 看到 `-` vs `skip`：`-` 跟亲和无关；`skip` 是 Claude 请求但没满足条件（多半客户端没启 cache），可排查客户端。两次完全相同请求若连续 miss → miss，对比 `request_body` 找差异。

## 性能

`go test -bench=. ./internal/affinity/` 实测（Apple M5）：

| 路径 | 单次耗时 |
|---|---|
| 非 Claude 早退（regex 匹配失败）| 2.3 ns |
| Claude 但未启用 cache（被 `hasCacheControl` 拦） | 1.2 µs |
| Compute 中等请求（~4.5KB，10 turns）| 50 µs |
| Compute 大请求（~18KB，50 turns）| 178 µs |
| Store Lookup / Record | ~80 / 90 ns（Memory），+1 RTT（Redis）|

合格请求开销 < Claude API 延迟的 0.1%；不相关流量基本零开销。

## 代码位置

| 文件 | 内容 |
|---|---|
| [internal/affinity/affinity.go](internal/affinity/affinity.go) | `Fingerprinter` / `Provider` 接口、Store 实现、按 channel_type 注册、Status 常量 |
| [internal/affinity/claude.go](internal/affinity/claude.go) | `ClaudeFingerprinter` 实现：门控、归一化、指纹 |
| [internal/affinity/*_test.go](internal/affinity/) | 单测 + bench |
| [internal/keypool/provider.go](internal/keypool/provider.go) | 新增 `GetKeyByID`（与 `SelectKey` 共享 `loadKeyByID`）|
| [internal/container/container.go](internal/container/container.go) | dig 注册 `affinity.NewProvider` |
| [internal/models/types.go](internal/models/types.go) | `RequestLog.AffinityStatus` 字段 |
| [internal/proxy/server.go](internal/proxy/server.go) | 接入点：`tryAffinityKey` / `executeRequestWithRetry` 流程 / attemptStatus 透传 |
| [web/src/components/logs/LogTable.vue](web/src/components/logs/LogTable.vue) | 表格列 + 详情 chip |
| [web/src/locales/](web/src/locales/) | 中/英/日 i18n |

## 扩展（OpenAI / Gemini）

后续支持其他 channel，无需改 affinity 包接口或 proxy 接入：

1. 在 [internal/affinity/](internal/affinity/) 新增 `openai.go` / `gemini.go`，实现 `Fingerprinter` 接口
2. 在 `NewProvider` 的 `fps` map 追加 `"openai": newOpenAIFingerprinter()`
3. 视需要追加 `OPENAI_AFFINITY_*` 等环境变量

每种 channel 的指纹规则需独立设计（OpenAI 没显式 cache_control 概念，Gemini 字段结构不同）。

## 验证

```
# 单测 + bench
go test ./internal/affinity/... -v
go test -bench=. -benchmem -run=^$ ./internal/affinity/

# 本地启动
CLAUDE_AFFINITY_ENABLED=true CLAUDE_AFFINITY_TTL=600 go run .
```

行为验证：连续两次相同 `/v1/messages` 请求（带 cache_control）→ 第一次 miss、第二次 hit；改 system/tools/first_user 任一字段 → miss；只改 cache_control 位置或加减 → 仍 hit。

## 已知边界

- **亲和 TTL > 服务端 cache TTL**：默认 1 小时 > 5 分钟。命中亲和但服务端 cache 已过期时仍复用同一 key（写一次新 cache），比换 key 写多个 workspace 强。严格对齐可把 TTL 调到 300。
- **仅 input_schema 差异不区分**：`(name, description, type)` 全相同但 input_schema 不同的两个工具会撞同一 fp。详见"为什么不抓 input_schema"——这是为避免客户端序列化器差异打击指纹稳定性主动做的取舍。
- **客户端在 system / first_user 注入动态内容**会让指纹永远漂（如时间戳、session id）。Claude Code 注入的"当前日期"是天级别，同一天内稳定；跨天会失效一次（可接受）。
- **MCP 不需要专门处理**：装/卸 MCP server 后，MCP 服务说明会进入 system reminder（messages[0] 的 text 块），MCP 工具会加入 `tools[]` 数组——两条都自然被 fp 捕获。
- **跨 group 隔离**：亲和命中后校验 `key.GroupID == group.ID`，避免脏数据导致跨 group 借用。
