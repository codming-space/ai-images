# Claude Affinity 设计

## 背景

`gpt-load` 默认按轮询从 key 池中分配上游 key（[internal/keypool/provider.go:38](internal/keypool/provider.go#L38) 的 `store.Rotate`）。Claude prompt cache 按 **workspace 隔离**且 TTL 较短（5 分钟默认 / 1 小时扩展），当 key 来自不同账户或组织时，轮询会让相同前缀的请求散落到不同 workspace，等于重复写缓存（1.25× 价格）且命中率几乎为零。

Affinity 在不破坏整体负载均衡的前提下，让"相同内容的请求"绑定到上次成功的同一个 key，最大化 prompt cache 命中率。

当前只对 Anthropic Claude 实现，代码结构预留 OpenAI / Gemini 扩展点。

## 只读保证

本功能对请求体 / URL / Header / 参数零修改：

- 复用 [internal/proxy/server.go:97](internal/proxy/server.go#L97) 已经 `io.ReadAll` 出来的 `bodyBytes`，只读不写
- 指纹归一化（剔除 cache_control、tools 排序、system 拼接）全部在 affinity 包内部的局部变量上做
- 发往上游的字节仍来自 [internal/proxy/server.go:153](internal/proxy/server.go#L153) 的 `bytes.NewReader(bodyBytes)`，与原始字节一致
- 输出仅一个 sha256 字符串 + 选定的 key_id

现有的 `ApplyModelRedirect` 是项目原有逻辑，不在本功能范围。

## 配置

两个环境变量（默认关闭，关闭时行为与本功能引入前完全一致）：

```
CLAUDE_AFFINITY_ENABLED=false    # 总开关
CLAUDE_AFFINITY_TTL=3600         # 映射 TTL（秒），默认 1 小时
```

加载位置：[internal/affinity/claude.go](internal/affinity/claude.go) 的 `newClaudeFingerprinter`，启动时读一次。

不修改 `types.ConfigManager` 接口，也不动数据库 schema、前端 UI 或 Group 配置。

## 匹配规则（硬编码）

只对同时满足以下条件的请求启用：

- `group.ChannelType == "anthropic"`
- `model` 匹配正则 `^claude-.*$`
- `path` 精确等于 `/v1/messages`
- body 是合法 JSON
- 请求**显式启用了 prompt cache**（详见下方"前提：必须启用 prompt cache"）
- `messages` 第一条 `role=user` 且包含至少一个 `type=text` 块（详见"边界处理"）

任一条件不满足 → 跳过亲和，走原轮询。

## 前提：必须启用 prompt cache

按 Anthropic 文档，prompt caching 通过 `cache_control` 字段启用，分两种模式：

| 模式 | 位置 |
|---|---|
| Automatic caching | 请求**根级** `cache_control` 字段（与 `model`、`messages` 平级） |
| Explicit breakpoint | `system[].cache_control` / `tools[].cache_control` / `messages[].content[].cache_control` |

[`hasCacheControl`](internal/affinity/claude.go) 扫这四个位置：

- 任一处出现 `cache_control` 且不为 `null` → 视为启用，继续亲和流程
- 都没有 → 跳过亲和（请求本来就不走 cache，绑定到同一 key 没有收益，反而限制负载均衡）

注意这跟"指纹归一化剔除 cache_control"**不冲突**：
- **检测**用：判断请求是否启用 cache，决定要不要做亲和
- **指纹**用：剔除 cache_control，让加减断点不影响指纹（同一会话稳定）

这条由 `TestClaude_Compute_RequiresCacheControl` 锁住，覆盖四个启用位置 + null 与缺失两种"未启用"形态。

## 指纹算法

`sha256( "strict" | model | normalized_system | canonical_tools | first_user_text )`

字段之间用 `\x00` 分隔 key/value，`\x01` 结尾，避免拼接歧义。

### normalized_system

- `system` 为字符串 → 直接使用
- `system` 为数组 → 仅取 `type=text` 块，按出现顺序 `\n` 拼接
- **`cache_control` 字段被剔除**——加减缓存断点不应影响绑定

### canonical_tools

- 按 `name` 升序排序后拼接（避免数组顺序影响）
- 每个工具取 `(name, description, input_schema)` 三元组
- 分隔符 `0x1f`（字段间）、`0x1e`（工具间）
- `input_schema` 用 `json.RawMessage` 原始字节（同一客户端的序列化器输出稳定）
- `cache_control` 被剔除

### first_user_text

- `content` 为字符串 → 直接返回
- `content` 为数组 → 仅取 `type=text` 块按出现顺序 `\n` 拼接
- `image` / `tool_result` / `tool_use` 等被忽略

### 边界处理：first_user_text 为空

`first_user_text == ""` 时 `Compute` 返回 `matched=false`，请求跳过亲和。

理由：若强行用空串入指纹，会把 `system + tools` 相同的所有不同用户 / 不同会话塌缩到同一 fp、错误绑到同一 key，破坏负载均衡。Claude API 无状态，会话首条 user text 在生命周期内稳定，所以这种情况只发生在"纯图像 / 纯工具回合首请求"等极少数场景，可接受这部分不享受亲和。

### 边界处理：system / tools 为空

`system` 或 `tools` 为空（字段缺失、`null`、`""`、`[]` 任意一种）**不会跳过亲和**——只要 `first_user_text` 非空就照常算指纹，覆盖"只有用户消息没有 system / 没有 tools"的简单聊天场景。

四种"空表达"会归一化到同一个空字符串，进而产生相同的指纹。这一点由 `TestClaude_Fingerprint_EmptySystemAndToolsRobustness` 锁住。

> 与官方定义的差异：Anthropic 文档说 prompt cache 引用 "tools → system → messages 直到 cache_control 断点"的完整前缀。我们的指纹只取到 first_user_text，比官方 cache 的匹配范围更窄；意味着相同 fp 下后续 message 的差异仍能命中我们的亲和，但 Claude 服务端不一定能命中 prompt cache。这是可接受的——亲和保证"落到同一个 workspace"，能让那部分确实命中 cache 的前缀复用，比每次随机换 key 强。

## 存储

复用 [internal/store/store.go](internal/store/store.go) 抽象，Redis 与 MemoryStore 都已支持 `SetNX` / `Get` / `Delete`，沿用 `gpt-load:` 命名空间。

```
key:   gpt-load:affinity:v1:{group_id}:{fingerprint}
value: {key_id}（字符串数字）
ttl:   CLAUDE_AFFINITY_TTL
op:    SET NX EX（先到先得）
```

## 选 key 与重试流程

接入点 [internal/proxy/server.go:117](internal/proxy/server.go#L117) `executeRequestWithRetry`，新增两个透传参数 `affinityFP / affinityHit`。

```
首次尝试 (retryCount == 0)：
  1. provider.Fingerprinter(group.ChannelType) → 拿到 Fingerprinter
  2. fingerprinter.Compute(model, path, body) → fp，matched=false 则跳过
  3. provider.Lookup(groupID, fp) → keyID
     ├── 命中且 keypool.GetKeyByID(keyID) 返回 Status=active 且 GroupID 一致
     │     → 直接使用该 key，affinityHit=true
     ├── 命中但 key 失效（不存在 / invalid / 跨 group） → Delete 映射，回退轮询
     └── 未命中 → 回退轮询
  4. affinityFP 透传给后续重试

重试 (retryCount > 0)：
  跳过亲和分支，正常 SelectKey 轮询
  affinityFP / affinityHit 通过递归参数透传

响应处理：
  - 失败（被判定为可重试的上游错误）且 affinityHit==true
        → 立即 Delete 映射并把 affinityHit 置为 false
        （不等 isLastAttempt，避免后续 Record(SETNX) 因旧映射存在而 no-op，
         也避免下一个同指纹请求继续命中已失败的 key）
  - 最终成功 (status < 400) 且 affinityFP 非空
        → provider.Record（SETNX）
        （若上一步已 Delete，这里能成功写入新的成功 key；否则保持原映射）
  - 最终失败 (isLastAttempt)：不需要额外处理，命中场景的 Delete 在失败那一刻就做了
```

### 何时设置亲和映射

写入由 `provider.Record`（SETNX 语义）完成，**仅一处触发**：成功响应处理之后。

两个条件同时满足才进入写入：

1. 请求最终落到成功分支（`status < 400`，且非 `IsIgnorableError` 提前 return 的情况）
2. `affinityFP != ""`——意味着请求通过了 `Compute` 的全部资格检查（Claude 模型 / 路径 `/v1/messages` / JSON 合法 / 含 `cache_control` / `first_user` 非空）

| 场景 | 写入行为 |
|---|---|
| 未命中 + 首次成功 | SETNX 建立新映射 |
| 未命中 + 重试链最终成功 | SETNX 建立到最终成功 key 的映射 |
| 命中 + 首次成功 | SETNX no-op（旧映射保留）|
| 命中 + 首次失败（立即 Delete） + 重试成功 | SETNX 建立到新 key 的映射 |
| 命中但 key 失效（`tryAffinityKey` Delete）+ 轮询成功 | SETNX 建立到新 key 的映射 |
| 任意场景下最终失败 | 不进入成功分支，不写入 |
| `Compute` 返回 `matched=false` 的请求 | `affinityFP == ""`，不写入 |

### 何时删除亲和映射

总共三处会 Delete：

1. **`tryAffinityKey` 内**：命中了但对应 key 已失效（`store.ErrNotFound` / `Status != active` / `GroupID` 不匹配）。属于"绑定到已失效的 key"，立刻清掉，让本次请求和下次请求都能轮询到新 key。
2. **失败响应处理时（`affinityHit == true`）**：命中的亲和 key 这次失败了。无论后续重试是否救回来，旧绑定都不该保留——重试成功时由 Record(SETNX) 写入新 key，重试失败时映射本来也没用。
3. （隐式）不再有第三处——之前的 `isLastAttempt + affinityHit` 逻辑在修复后变成不可达，已移除。

注意：`IsIgnorableError`（客户端主动断开等）不算上游失败，**不会**触发 Delete，亲和绑定保留。

### 关键不变式

把上面两小节对照看，可以得到三条不变式：

1. **绑定的 key 必然是某个时刻成功过的 key**——失败 key 不会被绑定（命中失败时立即 Delete）
2. **同一 fp 最多只有一个有效绑定**——SETNX 保证不抖动，失败时 Delete 保证不残留
3. **请求只在显式启用 prompt cache 时才建立映射**——`hasCacheControl` 门控让无收益的请求不污染存储

### SETNX 的语义

写入用 SETNX 而非 SET：

- 已存在映射 → 不覆盖，避免并发场景下的"最后写赢"抖动
- 命中且成功 → Record 实际是 no-op（映射已存在）
- 命中后失败 → 上面规则 2 已 Delete，Record 写入新 key
- 未命中 → Record 第一次写入

### 重试期间不再查亲和

`retryCount > 0` 时主动跳过亲和分支。否则上一次失败的 key 还会被命中并选中，导致死循环失败。

## 代码位置

| 文件 | 内容 |
|---|---|
| [internal/affinity/affinity.go](internal/affinity/affinity.go) | `Fingerprinter` / `Provider` 接口、`StoreProvider` 实现、按 channel_type 注册、`StatusNone / Miss / Hit / Unbind` 常量 |
| [internal/affinity/claude.go](internal/affinity/claude.go) | `ClaudeFingerprinter` 实现：匹配规则、归一化、指纹 |
| [internal/affinity/claude_test.go](internal/affinity/claude_test.go) | 归一化与边界单测 |
| [internal/affinity/affinity_test.go](internal/affinity/affinity_test.go) | Provider 单测（SETNX / Lookup / Delete） |
| [internal/affinity/bench_test.go](internal/affinity/bench_test.go) | 性能基准测试 |
| [internal/keypool/provider.go](internal/keypool/provider.go) | 新增 `GetKeyByID`，与 `SelectKey` 共享 `loadKeyByID` |
| [internal/container/container.go](internal/container/container.go) | 注册 `affinity.NewProvider` 到 dig 容器 |
| [internal/models/types.go](internal/models/types.go) | `RequestLog.AffinityStatus` 字段（用于日志可视化）|
| [internal/proxy/server.go](internal/proxy/server.go) | `ProxyServer.affinityProvider` 字段；`executeRequestWithRetry` 接入；`tryAffinityKey` / `affinityTTL` 辅助方法；attemptStatus 透传到 `logRequest` |
| [web/src/types/models.ts](web/src/types/models.ts) | `RequestLog.affinity_status` 类型 |
| [web/src/components/logs/LogTable.vue](web/src/components/logs/LogTable.vue) | 表格列 + 详情字段（chip 显示 hit/miss/unbind）|
| [web/src/locales/](web/src/locales/) | 中/英/日 i18n 文案 |

## 日志可视化

`RequestLog.affinity_status`（数据库字段 `affinity_status`）反映该次 attempt 的亲和处理结果，前端日志页用 chip 展示：

| 值 | 含义 | UI |
|---|---|---|
| `""` | 未启用 / 非 Claude / 不满足资格 | 显示 `-` |
| `miss` | 未命中（如果请求成功，已写入新映射）| default chip |
| `hit` | 命中已有映射并使用该 key | success chip |
| `unbind` | 命中后失败或失效，已删除映射 | warning chip |

注意状态是**每次 attempt** 一条：重试场景下，首次可能是 `hit→unbind`，重试那条是空（重试时不查亲和）。这样能在日志里看到完整的 attempt 链。

## 扩展（OpenAI / Gemini）

后续支持其他 channel 类型，按以下步骤即可，无需改动 affinity 包的接口或 proxy 接入：

1. 在 [internal/affinity/](internal/affinity/) 新增 `openai.go` 或 `gemini.go`，实现 `Fingerprinter` 接口
2. 在 [internal/affinity/affinity.go](internal/affinity/affinity.go) 的 `NewProvider` 中向 `fps` map 追加一行注册：
   ```go
   "openai": newOpenAIFingerprinter(),
   "gemini": newGeminiFingerprinter(),
   ```
3. 视需要追加对应环境变量 `OPENAI_AFFINITY_*` / `GEMINI_AFFINITY_*`

每种 channel 的指纹规则需要根据其 prompt cache 机制单独设计（如 OpenAI 没有显式 cache_control，Gemini 的字段结构与 Claude 不同），归一化逻辑必须独立。

## 验证

### 单测

```
go test ./internal/affinity/... -v
```

覆盖：
- `Compute` 命中条件（model / path / body 合法性）
- 空 first_user_text 不匹配（messages 空、首条非 user、content 全是 image / tool_result）
- 指纹对 `cache_control` 加减不变
- 指纹对 tools 数组顺序不变
- 内容变化（model / system / tools / first_user）产生不同指纹
- Provider 的 SETNX / Lookup / Delete 语义

### 本地行为

```
CLAUDE_AFFINITY_ENABLED=true CLAUDE_AFFINITY_TTL=600 go run .
```

- 用 curl 连续发两次相同 `/v1/messages` 请求，model=`claude-sonnet-4-5`
  - 第一次：日志显示 `affinity miss` → 记录映射
  - 第二次：日志显示 `affinity hit fp=... key=...`，使用与第一次相同的 key
- 只在 body 里加 `cache_control` 字段 → 仍然 hit
- 改 system / tools / 第一条 user text → miss
- 把命中的 key 拉黑后再发 → 日志显示 `affinity: delete stale mapping`，回退轮询
- 关闭开关 `CLAUDE_AFFINITY_ENABLED=false` → 完全恢复纯轮询行为

## 边界与权衡

- **亲和 TTL > prompt cache TTL**：默认 3600 秒 > Claude 默认 cache 300 秒。命中亲和但 cache 已过期时仍复用同一 key（写一次新 cache），比换 key 重写多 workspace 至少省了重复写成本。需要严格对齐时把 `CLAUDE_AFFINITY_TTL` 调到 300 即可。
- **并发首次请求**：相同指纹同时到达 → 都未命中 → 各自轮询 → 都 SETNX，第一个赢，后续请求会命中第一个赢家。可接受。
- **input_schema 字节稳定性依赖客户端**：`input_schema` 用 raw JSON 字节，同一客户端的序列化器（如 Claude CLI）输出稳定，跨客户端可能出现"语义相同但字节不同"导致指纹不一致。Claude CLI 场景下不是问题。
- **跨 group 隔离**：亲和 key 命中后会校验 `key.GroupID == group.ID`，避免脏数据导致跨 group 借用。
