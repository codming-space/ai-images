# Claude 亲和路由 — 缓存机制说明

## 背景

gpt-load 是多 key 负载均衡代理。Anthropic Claude API 有 prompt cache 机制，但缓存是 **按 workspace（实际相当于 organization / 账户）隔离的**，跨账户绝不共享。当 key 池里的 key 来自不同账户时，朴素轮询会让同一段 prompt 在多个账户之间反复 cache miss，浪费写入成本（1.25x ~ 2x base price）并增加 TTFT。

本机制通过 **会话级亲和路由** 解决这个问题：把同一个会话（或同一段共享前缀）粘到同一个 upstream key 上，让那个 key 的 cache 持续累积复用。

## 官方文档关键事实

来源：<https://platform.claude.com/docs/en/build-with-claude/prompt-caching.md>

| 事实 | 影响 |
|---|---|
| 缓存键 = `Hash(tools + system + messages[0..N])`，需 100% 字节级一致 | 任意字段变动都会失效 |
| 缓存隔离粒度：workspace 内（不同 org 永远不共享） | 多账户 key 池下，cache 无法在 key 之间共享 |
| TTL 选项：5min（默认）/ 1h（`cache_control.ttl: "1h"`） | 我们的亲和 TTL 取 1h 对齐 |
| 最小可缓存前缀：Opus 4.x 是 4096 tokens，Sonnet 4.6 / Haiku 4.5 是 2048/4096 | 短 prompt 不会被缓存，亲和路由也无意义 |
| Lookback window：往前 20 个 block 找已有 cache | 客户端长对话需要第二个 breakpoint 续接 |
| 触发缓存失效的字段：tools 内容变化、system 内容变化、tool_choice、images、thinking 参数等 | 我们 hash 时按这些字段的规约形式参与 |

## 设计

### 双档位

亲和键计算分两档（strict / loose），由请求 body 内容决定：

**strict — 会话粘连，hash 包含 first_user 文本**

任一触发即归 strict：
- `thinking.type` 非 `disabled`（顶层）
- `tools` 数组非空（顶层）
- `messages[].content[]` 中出现 `thinking` / `redacted_thinking` / `tool_use` / `tool_result` / `server_tool_use` block
- `messages[].content[]` 中任意 block 带 `cache_control`

理由：
- `tool_use_id` / thinking signature 跨 org 失效（不同账户的 ID 空间和签名密钥独立），换 key 会出错，不仅仅是 cache miss
- message-level cache_control 的缓存前缀含会变的对话历史，不存在跨用户复用的可能

因此这些请求 **必须** 在 1h 内粘在同一个 key 上。包含 `first_user` 到 hash 里是为了让不同会话散到不同 key（保留负载均衡）。

**loose — 前缀共享，hash 不含 first_user**

触发条件：仅顶层 `system` 或 `tools` 带 cache_control，且无任何 strict 触发。

理由：被缓存前缀是 system / tools 段，不随对话内容变化。把"同 system 的多个会话"塌到同一个 key，能让该 key 的 system prefix cache 持续命中。

副作用：牺牲了"同 system 流量"的负载均衡。但 Claude Code 这类客户端都带 `tools`，会优先走 strict，loose 实际很少触发。

### TTL = 1h

对齐 Anthropic `cache_control.ttl: "1h"` 的最大值。如果客户端用默认 5min cache_control，亲和路由仍粘连 1h，超过 5min 后只是 cache 过期重写，路由本身仍正确。

### 字节级 hash 稳定性承诺

7 个核心纯函数（`Inspect` / `hasCacheControl` / `normalizedSystem` / `canonicalTools` / `firstUserText` / `computeKey` / `writeKV`）的输入输出 **必须跨版本字节级稳定**。任何改动都会让线上正在跑的会话 hash 漂移，全部 cache miss。

升级 hash 逻辑需要灰度：双写新旧映射 → 等旧 TTL 过期 → 下线旧逻辑。

## 实现

### 文件清单

| 文件 | 职责 |
|---|---|
| [internal/keypool/claude_affinity.go](internal/keypool/claude_affinity.go) | Hash 计算（`Inspect` + 6 个纯函数），不依赖 gin/logrus |
| [internal/keypool/provider.go](internal/keypool/provider.go) | `SelectKeyByAffinity` 查映射、`RememberAffinity` 写映射 |
| [internal/proxy/server.go](internal/proxy/server.go) | `HandleProxy` 计算 affinity_key，`executeRequestWithRetry` 选 key |
| [internal/models/types.go](internal/models/types.go) | `RequestLog` 增加 `AffinityTier` / `AffinityHit` 列 |

### 数据流

```
HandleProxy
  ├── 读取 body
  ├── 应用 param_overrides → finalBodyBytes
  └── if CLAUDE_AFFINITY_ENABLED && group.ChannelType == "anthropic":
        result := keypool.Inspect(finalBodyBytes)
        if result.AffinityKey != "":
            c.Set("claude_affinity_key", result.AffinityKey)
            c.Set("claude_affinity_tier", result.Tier)

executeRequestWithRetry
  ├── c.Set("claude_affinity_hit", false)   // 每次尝试重置
  ├── if retryCount == 0 && affinityKey != "":
  │     apiKey = SelectKeyByAffinity(groupID, affinityKey)
  │     if apiKey != nil: c.Set("claude_affinity_hit", true)
  ├── if apiKey == nil:
  │     apiKey = SelectKey(groupID)         // 原本轮询
  │     if retryCount == 0 && affinityKey != "":
  │         RememberAffinity(groupID, affinityKey, apiKey.ID, 1h)
  └── 发起上游请求

logRequest
  └── 把 tier / hit 写入 RequestLog
```

### Redis 键格式

```
affinity:claude:group:{groupID}:hash:{sha256_hex}
```

值是 keyID 的 ASCII 字符串。TTL 1h。

### 重试行为

仅 `retryCount == 0`（首次尝试）使用亲和路由。重试时跳过，避免反复打到失败 key。每次 `executeRequestWithRetry` 入口处 `c.Set("claude_affinity_hit", false)`，确保递归重试帧不会继承上一帧的 hit=true。

### 失败安全

- `Inspect` 内部包了 `defer recover()`：panic 则退化到轮询，业务请求不受影响。
- `SelectKeyByAffinity` 检查目标 key 必须仍是 `active`：被黑名单的 key 自动失效，回到轮询。
- 新选出的 key 覆写映射：自然恢复。

## 触发决策表

| 请求特征 | tier | hash 字段 |
|---|---|---|
| 普通 chat，无 tools / thinking / cache_control | — | 不写亲和键，走轮询 |
| 带 `tools` 数组 | strict | tier + model + system + tools + first_user |
| 带 `thinking.type: enabled` | strict | 同上 |
| messages 含 `tool_use` / `tool_result` block | strict | 同上 |
| messages 含 message-level `cache_control` | strict | 同上 |
| 仅 `system[].cache_control`，无其他触发 | loose | tier + model + system + tools |
| 仅 `tools[].cache_control`，无其他触发 | loose | 同上 |
| 任意请求带 `metadata.user_id` | — | 短路跳过（交给上游/外层按 user_id 处理） |
| loose 命中但 system/tools 都为空 | — | 跳过（防 hash 退化到 tier+model 常量） |
| strict 命中但 first_user 为空 | — | 跳过 |

## 配置

### 启用

```bash
export CLAUDE_AFFINITY_ENABLED=true
./gpt-load
```

启动日志：
```
INFO Claude affinity routing: ENABLED (TTL=1h, anthropic channels only)
```

### 关闭（默认）

不设置环境变量。行为与改造前完全一致。

环境变量启动时读取一次，改动需要重启进程。

## 可观察性

### 日志（DEBUG 级别）

```
DEBU Claude affinity key computed tier=strict triggers=[cache] key=4a8f9c1d2e3b6071 model=claude-opus-4-7
DEBU Claude affinity HIT groupID=1 keyID=42 key=4a8f9c1d2e3b6071
```

### 请求日志详情面板

`RequestLog` 表的 `affinity_tier` / `affinity_hit` 列：

- `tier` 非空，`hit=true` → 复用了已有映射
- `tier` 非空，`hit=false` → 首次写入新映射
- `tier` 空 → 该请求未触发亲和路由

前端「请求详情」基本信息卡里会显示一行 `亲和路由: [tier] · 命中/新建映射`，未触发时隐藏。

## 验证

### 编译

```bash
go build ./internal/...
go vet ./internal/...
```

### 端到端

把测试脚本 ANTHROPIC_BASE_URL 指向 gpt-load 实例（`/proxy/<group>/v1/messages`），跑两次相同 body：
- 第一次：日志 `Claude affinity key computed`，无 HIT。`affinity_hit=false`。
- 第二次（1h 内）：日志 `Claude affinity HIT`，路由到同一个 key。`affinity_hit=true`。

## 跨账户场景的预期对齐

**你的 key 池里的 key 来自不同账户，意味着：**

1. 不同 key 之间 cache 绝对不共享。亲和路由的价值是 **让同一会话在一个 key 上累积 cache**，不是让多 key 共享 cache。
2. 首次任何 affinity_key 都是 cache miss（要写入）。第二次起才命中。
3. 切换客户端 / 修改 system prompt → affinity_key 完全不同 → 需要重新建立 cache。
4. 想让 1h TTL 真正发挥作用，客户端的 `cache_control` 要带 `ttl: "1h"`。Claude Code CLI 默认会带，自建客户端需检查。

## 风险与边界

| 风险 | 处理 |
|---|---|
| 亲和的 key 被黑名单 | `SelectKeyByAffinity` 检查 status，自动 fallback 轮询；新选出的 key 覆写映射 |
| 单 key rate limit | TTL 1h 内同一 affinityKey 始终路由同一 key。如果该会话流量极大可能压垮单 key |
| `Inspect` panic | `defer recover()` 退化轮询 |
| Hash 字段意外改动 | 见上文「字节级 hash 稳定性承诺」，需要灰度 |
| 客户端 prompt 长度 < 最小可缓存阈值 | Anthropic 不会缓存，亲和键照常分配但没收益 |

## 起源

移植自 <https://github.com/QuantumNous/new-api> 的 `claude_affinity` 中间件。
相比原版的差异：

| 项 | new-api | gpt-load 移植版 |
|---|---|---|
| `Inspect` + 6 个纯函数 | 同 | 同（字节级一致） |
| `token_id` salt | 有 | **去掉**（单用户多账户场景，加 salt 反而降低跨对话缓存复用） |
| metadata.user_id 短路 | 有 | 保留 |
| TTL | 后台配置 | 硬编码 1h |
| 注入方式 | gin 中间件 | 直接在 `HandleProxy` 内调用 |
| 路由实现 | new-api 内置 channel_affinity | `SelectKeyByAffinity` |
| 开关 | 后台配置 | `CLAUDE_AFFINITY_ENABLED` 环境变量 |
