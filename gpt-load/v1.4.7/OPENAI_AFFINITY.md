# OpenAI Responses 路由亲和设计

状态：已完成本地实现与测试，未提交，尚未验证真实上游缓存收益。基于本地 v1.4.7 及现有 Claude 亲和实现，不涉及上游 v2。官方文档核对日期：2026-09-21。

## 目标与范围

在同一个实际分组内，将具有相同稳定前缀的无状态 Responses 请求，优先分配到此前使用成功的 API key，减少轮询造成的缓存分散。指纹使用四项：**实际模型、系统提示词文本、简化后的工具列表、首条用户输入文本**。每个工具只取 `(name, description, type)` 三元组；Schema、运行参数和后续对话不参与，避免这些字段变化时更换绑定。

仅支持 `openai-response` 通道的 `POST /v1/responses`，包括普通 JSON 响应和 HTTP SSE。复用现有 `affinity.Provider`、Store、密钥状态检查和日志字段。默认关闭，通过独立环境变量启用。

不处理 `previous_response_id`、`conversation` 等上游会话状态，不建立 response ID 到 key 的映射，不覆盖 Chat Completions、WebSocket、后台任务查询、Responses 检索或 `/v1/responses/compact`。

亲和模块只读取请求。参数覆盖、模型重定向、鉴权及自定义请求头仍按项目现有规则处理；亲和本身不改写正文、URL 或 Header，不注入 `prompt_cache_key`、缓存断点或缓存保留参数。

## 官方缓存规则与本地设计的关系

以下依据官方 Prompt caching 指南 [1]，本地指纹规则则属于本项目的设计选择。

| 官方行为 | 设计含义 |
|---|---|
| 缓存复用要求渲染后的前缀匹配，工具、指令和部分模型设置也会影响前缀 | 本地使用稳定文本与简化工具列表决定 key 绑定；忽略 Schema 和运行设置的变化，实际缓存复用由上游判断 |
| 缓存存在于上游机器，不跨组织或处理区域共享 | 固定 API key 可减少账户切换，但官方未承诺缓存按单个 API key 隔离；同组织的多个 key 不一定需要亲和 |
| 最低可缓存长度和断点规则取决于模型；当前 GPT-5.6 及以后模型为至少 1,024 个可见输入 token | 不用字符数猜测是否达到缓存门槛，不把固定 token 数写成所有模型通用的资格条件 |
| 较早模型的稳定 `prompt_cache_key` 有助于缓存路由；GPT-5.6 及以后主要用于分开缓存核算 | 原值透传，不纳入本地指纹，避免客户端修改它时连带改变本地 key 绑定 |
| 较早模型的同一 cache key 流量过高可能分流；指南建议繁忙分区约 15 次请求/分钟 | 这属于上游缓存路由建议，不设置成 gpt-load 的限流值，也不据此自动切换 key |
| 缓存寿命和配置因模型而异，当前文档包含 `prompt_cache_retention` 与 `prompt_cache_options` 两套参数 | 本地映射 TTL 独立配置；不替客户端选择缓存模式、保留时间或承担额外缓存写入费用 |
| 实际读取的缓存 token 可从 `usage.input_tokens_details.cached_tokens` 观察 | `affinity_status=hit` 只说明用了绑定 key；实际缓存收益必须另行测量 |

### 简化方案的判断

本设计可以继续采用四项指纹，无需为匹配官方缓存规则而把所有参数加入 hash。本地指纹只决定选择哪个 API key，请求正文仍完整发送，上游独立检查实际渲染前缀。[1]

需要接受以下取舍：

- 工具 Schema、工具顺序、`reasoning.effort` 等变化时，本地指纹可以保持不变，但上游从变化位置开始的后续前缀可能无法复用。官方示意图中工具定义位于 developer 消息和对话历史之前，其变化可能影响较长的后续缓存；保留 key 绑定无法消除这个影响，也不能保证只损失少量缓存 token。
- 工具排序仅用于本地指纹，不重排发送给上游的工具。客户端要提高实际命中率，仍应尽量保持完整工具定义、工具顺序和相关设置稳定。
- 不同 Schema 或运行配置的请求可能共用一个绑定，增加单个 key 的请求量。这需要通过各 key 的请求数、429 和实际 cached tokens 验证，不能仅凭本地 hit 比例判断收益。

同一指纹不会直接复用本地响应内容；即使两个请求实际前缀不同，上游也会分别检查并执行。简化提取不会绕过上游缓存匹配，但收益和负载分布仍需实测。

对于兼容 Responses 的第三方上游，其账户划分、缓存及参数支持需要实测。这里不将 OpenAI 官方行为推定为所有兼容服务的行为。

## OpenAI 与 Codex 的缓存时长

### OpenAI 官方默认与建议

按 2026-09-21 核对的官方指南，缓存时长需要区分模型和参数：[1]

| 模型或策略 | 默认或支持情况 | 缓存时长 |
|---|---|---|
| GPT-5.6 及以后 | `prompt_cache_options.ttl="30m"`，当前默认且唯一支持值 | 最近一次写入或复用后至少 30 分钟，可能保留更久；复用会刷新寿命 |
| 较早模型的 `in_memory` | 仅在模型支持时可选；同时支持两种策略的模型，启用 ZDR 的组织默认使用它 | 通常闲置 5–10 分钟后过期，最多保留一小时 |
| 较早模型的 `24h` | 仅在模型支持时可选；未启用 ZDR 的组织默认使用它 | 通常可保留约 30 分钟，最长 24 小时；不能理解为保证每条缓存都保留 24 小时 |
| GPT-5.5 / GPT-5.5 Pro | `prompt_cache_retention` 仅支持 `24h` | 同上，最长 24 小时 |

ZDR 指 Zero Data Retention。官方在 **2026-05-29** 将未启用 ZDR 组织的 `prompt_cache_retention` 默认值从 `in_memory` 改为 `24h`。[4] 当前指南对较早模型的建议是：在模型及数据保留要求允许时优先使用 `24h`；GPT-5.6 及以后使用 `prompt_cache_options.ttl` 控制最短寿命。[1]

`30m` 表示最短可复用寿命，`24h` 表示最长保留策略，两者含义不同。代理不自动添加这些参数，也不因参数变化重新计算亲和指纹。

### Codex 客户端行为

本机独立 CLI 为 `0.155.1`，桌面应用内置核心为 `0.155.0-alpha.9.2`。本次源码结论限定于官方 `rust-v0.155.1` 标签的标准 Responses 请求路径：[5]

- `core/src/client.rs` 中的 `prompt_cache_key()` 通常返回当前会话 ID；显式覆盖及部分内部子任务另有处理。
- `build_responses_request` 会设置 `prompt_cache_key`；`codex-api/src/common.rs` 的 `ResponsesApiRequest` 没有 `prompt_cache_retention` 或 `prompt_cache_options` 字段。该路径没有自行指定 5 分钟、一小时或 24 小时的 TTL。
- 使用标准 OpenAI API 且中间代理没有覆盖缓存参数时，适用该模型和组织的 API 默认规则。使用其他兼容上游时，依赖该上游的实现。
- ChatGPT 登录模式使用其后台服务。公开客户端代码不足以确认该后台的统一缓存 TTL；不能将 API 的默认值直接当作所有 Codex 登录方式的保证。[6]

这里核对了公开源码和本机版本，未捕获真实 Codex 请求，也未实测服务端缓存过期时间。桌面内置版本的具体行为仍需以其实际请求为准。

### 对本地亲和 TTL 的影响

`OPENAI_AFFINITY_TTL` 只控制“指纹 → API key”的映射。默认设为 `7200` 秒（两小时），保持现有 `SETNX` 实现；它不会让上游缓存保留两小时，也不会随上游缓存命中续期。

两小时用于减少本地绑定的过期频率，不要求与上游缓存寿命一致。本次不增加自动识别模型缓存时长、请求级 TTL 或滑动续期；绑定失效或请求触发既有解绑条件时，仍可提前重新选择 key。

## 当前代码与接入位置

| 代码位置 | 已确认行为 | 设计要求 |
|---|---|---|
| `internal/channel/openai_response_channel.go` | 通道注册名为 `openai-response` | 新指纹器注册到这个名称，保留 `openai` 通道的现有行为 |
| `internal/proxy/server.go` 的 `HandleProxy` | 先选择聚合分组的子分组，再执行 `applyParamOverrides` | 使用实际子分组 ID；根据参数覆盖后的正文计算指纹 |
| 同文件的 `tryAffinityKey` | 已提供指纹计算、查映射、key 状态及所属分组检查 | 在这里增加 Responses 的 HTTP 方法限制和实际模型解析 |
| 同文件的 `executeRequestWithRetry` | 亲和选 key 之后才调用 `ApplyModelRedirect` | 提前只读解析实际模型，不为了计算指纹再次改写请求 |
| `internal/affinity/affinity.go` | 按通道注册；`Get`、`SetNX`、`Delete` 管理映射 | 复用 Provider 和 Store 接口，为新算法设置独立指纹标识 |
| `internal/keypool/provider.go` | `GetKeyByID` 不轮换列表，`SelectKey` 通过 `Rotate` 轮询 | Responses 重试需要排除本次请求已经失败的 key |

### 实际模型

在 Responses 分支读取参数覆盖后的 `model`，按 `group.ModelRedirectMap` 做与 `BaseChannel.ApplyModelRedirect` 相同的一次精确查找，将目标模型作为 `Compute` 的 `model` 参数。指纹器只用这个参数，不能再用正文中的旧模型名覆盖它。

无匹配且非严格模式时使用原模型；重定向表非空且严格模式无匹配时跳过亲和，交给原有流程返回错误。重定向表为空时，依照当前 `ApplyModelRedirect` 的提前返回规则使用原模型。不改变模型重定向的执行位置和其他通道行为。两个别名指向同一实际模型时可以共用绑定；同一别名改指向其他模型后生成新指纹。

### 亲和范围

绑定单位为 **实际分组内的 API key**。聚合分组仍先按权重选子分组；`BaseChannel.getUpstreamURL` 仍独立按权重选上游 URL。因此本方案不保证同一请求前缀始终使用相同子分组或上游 URL。

验证收益时先使用标准分组、单一上游地址和固定的组织/项目请求头。一个分组的多个地址若对应不同组织、区域或缓存服务，同 key 仍可能无法复用缓存；需要按实际缓存范围拆分分组。动态 `OpenAI-Organization` / `OpenAI-Project` 请求头不属于本次正文指纹的区分范围。

## 配置与资格条件

```dotenv
OPENAI_AFFINITY_ENABLED=false
OPENAI_AFFINITY_TTL=7200
```

启动时读取一次，TTL 单位为秒。未配置、解析失败、非正数或转换为 `time.Duration` 会溢出时使用 7,200 秒；非法配置输出简短警告。不新增数据库字段、分组设置或前端开关，Claude 的配置保持独立。

满足以下全部条件才计算指纹，否则按现有轮询处理：

| 条件 | 规则 |
|---|---|
| 通道、开关 | `group.ChannelType == "openai-response"` 且开关已启用 |
| 方法、路径 | `c.Request.Method == POST`，`c.Param("path") == "/v1/responses"`；不使用带 `/proxy/{group}` 的完整路径 |
| 模型 | 覆盖及重定向后的模型为非空字符串；不维护 `gpt-*` 等模型白名单，以支持别名和兼容上游 |
| JSON | 正文为合法 JSON 对象；参与提取的字段类型可识别 |
| 上游状态 | `previous_response_id`、`conversation` 缺失或为 `null`；出现任何非 null 值均跳过，包括无效空串 |
| 本地可见内容 | 非 null 的托管 `prompt` 引用、`input` 任意位置的 `item_reference` 均跳过；不查询远程模板或历史内容 |
| 执行方式 | `background=true` 跳过；`store` 的 true、false 或缺失本身不影响资格 |
| 输入前缀 | 能提取下文定义的完整初始文本前缀，且首条用户消息至少有一个非空文本块 |

方法检查放在代理层，保持 `Fingerprinter.Compute(model, path, body)` 接口不变。`store=true` 只表示存储当前响应，本次请求若已提供完整输入仍可亲和；将来通过响应 ID 查询或续接不在本设计范围内。[2][3]

不要求 Anthropic 的 `cache_control`，也不要求客户端必须传入 `prompt_cache_key`。亲和层不验证所有模型的缓存开关和断点组合；即使请求未产生服务端缓存，正常转发也不依赖本地亲和是否生效。

## 指纹算法

```text
fingerprint = "openai-responses-v1:" + hex(SHA256(json_encode([
    effective_model,
    system_text,
    canonical_tools,
    first_user_text
])))
```

用固定顺序的四项组成 JSON 数组，再做 SHA-256，避免直接拼接字段的歧义。模型及两项文本为字符串，`canonical_tools` 为排序后的三元组数组。普通 JSON 编码即可，不需要递归规范化完整工具定义或 Schema。算法标识用于与 Claude 指纹隔离，实施后改变提取规则时再递增版本。它仅用于 gpt-load 内部。

其他参数发生变化时，优先保留原有 key 绑定。它们可能影响上游的实际缓存命中，但本地不因此主动更换 key；同一指纹允许对应不同的工具 Schema 或生成配置。

### 指令与首条用户输入

Responses 使用顶层 `instructions` 和 `input`，不能复用 Claude 的 `system` / `messages` 字段提取器。[2]

1. `instructions` 是字符串时保留全文；缺失、`null`、空串统一为空字符串。其他类型跳过。
2. `input` 是字符串时，直接作为 `first_user_text`。
3. `input` 是数组时，从开头读取连续的 `system` / `developer` 消息；紧接着必须是一条 `user` 消息，提取它的文本作为 `first_user_text`。
4. 消息的 `type` 可缺失或为 `message`。`content` 是字符串时直接取值；数组必须由可识别的 `input_text` 块组成，取各块的 `text`，忽略空字符串，再按原顺序用 `\n` 拼接。
5. 首条 user 之前出现 assistant、工具结果、reasoning、compaction 或未知类型时跳过，不继续向后搜索用户消息。初始前缀包含图片、文件、音频或其他非文本块时也跳过，避免丢失内容后错误归并。
6. 首条 user 之后的消息、工具调用、工具结果及完整内联 reasoning 内容不参与 hash。仍检查整个 `input` 是否包含 `item_reference`，以排除对上游存储内容的依赖。
7. 将 `instructions` 和开头各条 system/developer 消息的文本依次排列，忽略空字符串，用 `\n` 拼接为 `system_text`。全部为空时使用空字符串，仍可通过非空的 `first_user_text` 计算指纹。

系统与用户输入部分只保留提取后的文本，不额外区分指令来自 `instructions`、system 还是 developer，也不保留消息或文本块的边界。其他指纹字段相同时，拼接后文本相同就使用同一指纹，例如单个 `"A\nB"` 文本块与依次包含 `"A"`、`"B"` 的两个文本块。提取时仍检查消息角色和内容类型，不跳过未知内容后继续计算。

消息 ID、状态、缓存断点等元数据均忽略。首条用户文本为空或没有 user 时跳过，不退回到仅凭公共系统提示词绑定。资格检查与指纹字段分开：`previous_response_id` 等状态参数虽然不进入 hash，出现时仍按前文规则跳过亲和。

文本不做 trim、换行归一化、大小写转换、Unicode 归一化或任意长度截断。时间、会话标识、目录和环境描述若出现在文本中，全部保留；目前没有针对 OpenAI 客户端动态文本的已验证删除规则。JSON 转义形式不同但解码后相同的字符串，视为相同文本。

**选择首条 user 的取舍：**相比只按公共系统提示词绑定，不同首条输入可以分配到不同 key；完整历史中的追加内容又不会改变绑定。不同会话共用相同首条输入时仍会使用同一指纹。如果客户端第一条 user 固定为环境说明，实际问题放在第二条 user，本方案会按环境说明分组，不根据文本含义猜测“真正的问题”。

例如，相同配置下，`[developer D, user U1]` 与 `[developer D, user U1, assistant A1, user U2]` 使用同一指纹；将 D 或 U1 改掉则重新计算。只改 U2 不影响绑定。若客户端压缩历史并替换 U1，新请求会建立新的绑定。

### 简化工具列表

参考现有 Claude `canonicalTools` 的字段选择、排序和空值规则：

1. 每个 `tools[]` 顶层对象只取 `(name, description, type)` 三个字段。Responses 工具直接读取这三个同级字段，不增加 Chat Completions 的 `function.name` 兼容提取。
2. 字段为字符串时保留原文；缺失、`null` 或非字符串统一为空字符串。内置工具没有 name / description 时仍通过 type 区分。
3. 按 `(name, type, description)` 依次升序排序，完整三元组参与比较，消除工具数组顺序的影响。
4. 排序后的每个三元组编码为 `[name, description, type]`，保留重复项，不做去重。`tools` 缺失、`null` 或 `[]` 统一为 `[]`；非数组或数组项不是对象时跳过亲和。
5. 不读取 `parameters`、`input_schema`、`strict`、缓存标记及其他设置，也不展开嵌套工具或查询远端工具列表。

```text
canonical_tools = [
    ["", "", "web_search"],
    ["read_file", "Read a file", "function"]
]
```

实际编码使用 JSON，避免工具描述包含分隔符时产生拼接歧义。

工具顺序或 Schema 变化不改变指纹；增删工具或改变三元组中的任一字符串会改变指纹。工具描述中的动态文本仍会影响指纹，不自动删除。同一三元组下的内置工具设置、嵌套工具列表等差异会共用绑定，这是保持提取规则简单的取舍。

### 不参与指纹的字段

采用上述四项的固定白名单。除资格检查所需字段外，不解析其他字段来计算指纹；以后新增的请求参数默认也不参与。

| 字段 | 指纹处理 |
|---|---|
| 工具的 Schema、`strict`、嵌套工具和其他设置 | 忽略；工具数组顺序通过三元组排序消除，只有三元组及其数量参与 |
| `text`、`reasoning`、`parallel_tool_calls`、`tool_choice` | 全部忽略；输出格式、思考设置和工具选择变化不改变指纹 |
| `context_management`、`truncation` | 忽略配置；如果客户端实际改写了系统提示词或首条用户文本，则按新的文本计算 |
| `prompt_cache_key`、`prompt_cache_retention`、`prompt_cache_options`、内容块上的缓存断点 | 全部忽略，按原有代理规则透传 |
| `stream`、`store`、`include`、`metadata`、`user`、`safety_identifier`、`service_tier` | 全部忽略 |
| `temperature`、`top_p`、`max_output_tokens` 等生成参数 | 全部忽略 |

只需要对简化后的工具三元组排序，不需要 Schema 规范化、数字归一化或模型默认参数推断。JSON 对象键顺序、外部空白及等价字符串转义不影响提取后的文本；提示词文本内部的实际差异仍保留。

工具三元组相同但 Schema 或设置不同的请求可能共用一个绑定，实际缓存命中仍取决于完整上游请求。代理不缓存响应内容，每个请求继续由上游执行。工具改动如果同时改变了系统提示词文本，指纹也会变化；不从提示词正文中猜测并删除工具说明。

### `prompt_cache_key` 的使用原则

`prompt_cache_key` 不参与本地指纹，也不作为优先使用的替代键。

- 增加、删除或修改它，都不影响本地 key 绑定。
- 原值按现有代理规则转发，不自动生成、覆盖或清理。
- 上游仍会按照该字段处理自身缓存；本地忽略它不能消除它对上游缓存的影响。
- 本地映射按实际分组隔离，不用客户端 cache key 区分用户或分配凭据。

## 映射、选 key 与重试

复用现有存储格式：

```text
Store 逻辑键: gpt-load:affinity:v1:{actual_group_id}:{fingerprint}
值:           十进制 key_id
写入:         SET NX，TTL = OPENAI_AFFINITY_TTL
```

这里的指纹已带 `openai-responses-v1:`。`RedisStore` 还会额外加自己的 `gpt-load:` 前缀，本次保持现有约定，不顺带迁移 Claude 映射。Store 仅新增摘要和 key ID，不额外保存提示词、工具定义或明文凭据。

```text
首次尝试：
  通道未启用                         → 原轮询，日志 ""
  已启用但不满足资格                 → 原轮询，日志 skip
  无绑定 / Lookup 出错               → 原轮询，日志 miss
  绑定 key active 且属于当前分组      → 使用绑定 key，日志 hit
  绑定 key 不存在 / 失效 / 分组不符   → 删除绑定，再轮询，日志 unbind

上游结果：
  客户端取消等 IsIgnorableError      → 保留已有映射，不重试
  网络失败或命中原有 failover 配置    → 命中亲和的尝试删除旧绑定；按原重试预算继续
  未触发 failover 且 HTTP 200         → Record(SETNX)
  其他 HTTP 状态                     → 不新建映射，沿用原有响应处理

重试：
  沿用首次计算的指纹，不再次查亲和
  排除本次请求已经失败的 key，再轮询其他候选
  成功才尝试记录；无候选或预算用完则返回错误
```

存储读写失败不额外中断已可转发的请求；`GetKeyByID` 的暂时读取错误沿用现有解绑和轮询策略。共享 Store 本身不可用时，普通轮询也可能失败，不能承诺 Redis 故障时代理仍可服务。

### Responses 的两项局部调整

**只在 HTTP 200 后建立映射。**Responses 仅在未进入 failover 分支且 HTTP 状态为 200 时调用 `Record`，默认不重试的 404 不建立映射。非重试错误也不单凭该错误删除已有有效绑定。Claude 分支保持原有行为。

HTTP 200 是本阶段的绑定成功标准。现有代理在收到响应头后记录映射，随后直接转发正文，不解析 Responses 状态或 SSE 事件。因此 HTTP 200 内的 `response.failed`、生成中断及 `incomplete` 不会被完整识别，也不能宣称“生成完成后才绑定”。暂不为亲和增加响应缓冲和 SSE 解析。

**重试排除失败 key。**亲和命中通过 `GetKeyByID` 选 key，不改变轮询位置；仅跳过亲和查询不能保证重试换 key。符合 Responses 亲和资格的请求维护已失败 key ID 集合，并使用独立的 `SelectKeyExcluding`：读取一次当前 active 列表长度，最多轮询该数量的候选，跳过已失败、非 active、已删除或所属分组不符的 key。该方法遇到存储读取故障时返回错误，不增加 key 失败计数。集合只在该请求内有效；没有其他候选或无法选择下一个 key 时结束，不重新使用本次已失败 key，向客户端返回最后一次上游错误；初次选 key 就没有候选时仍返回原有无可用 key 错误。普通轮询和 Claude 重试继续使用原方法。

### TTL 与并发限制

- TTL 从首次 `SETNX` 成功开始计算，命中和重复 `Record` 均不续期。默认两小时之后，下一次请求可重新经轮询建立映射；持续对话也会受此影响。
- 本地映射存在不代表服务端缓存仍存在。延长本地 TTL 只能减少 key 变化，不能延长服务端缓存寿命。
- 多个并发首次请求可能分别选到不同 key，最先完成有效 `SETNX` 的请求建立绑定。这里不增加锁、预占绑定或一致性哈希，也不保证首次并发请求使用相同 key。
- 同一指纹的大量请求会集中到一个 key，按请求数量的均匀分配不再成立。本阶段不自动按负载拆分指纹；若缓存收益不足且 429 增加，可关闭开关恢复原轮询，或将不同业务分配到独立分组。修改客户端 `prompt_cache_key` 不会拆分本地绑定。
- 现有 `Delete` 没有“仅当当前值仍等于失败 key 才删除”的条件。并发旧请求失败时可能删除其他请求刚重建的绑定，导致额外 miss；`SETNX` 不能消除这个竞态。本阶段保留该限制，若实测频繁发生，再独立增加 Redis / MemoryStore 的原子条件删除。
- MemoryStore 的过期清理发生在访问相关键时，没有后台扫描。大量一次性指纹可能留下过期条目；持续或多实例部署优先使用共享 Redis。不同进程的 MemoryStore 不共享绑定。

## 日志与收益验证

复用现有 `affinity_status` 及 UI：`""`、`skip`、`miss`、`hit`、`unbind`。重试 attempt 继续使用空状态，首个命中尝试失败时记为 `unbind`。不新增数据库字段，也不将 `hit` 显示为上游缓存命中。

资格不满足的具体原因可输出到 debug 日志，例如 `stateful_request`、`unsupported_prefix`、`empty_first_user`，只记录原因类别及必要的分组信息，不新增提示词或客户端 cache key 原文日志。

实际收益由测试客户端从 JSON 响应的 `usage` 或 SSE 完成事件内的 response usage 采集；当前代理只转发数据，管理后台暂不新增 token 统计。[1]

| 指标 | 计算或观察方式 |
|---|---|
| 本地亲和命中比例 | 首次尝试的 `hit / (hit + miss + unbind)`，排除 skip 和重试 |
| 上游缓存 token 比例 | `sum(cached_tokens) / sum(input_tokens)`，不平均每个请求的百分比 |
| 缓存写入及费用 | 上游提供时同时记录 `cache_write_tokens`，结合实际模型费率计算，不能只看缓存读取 |
| 延迟与分配 | 客户端测量首 token 延迟、总耗时；按 key 检查请求数、429 和重试比例 |

先在固定模型和单一实际分组上回放完整历史：首个请求建立映射，后续请求追加消息；再与关闭开关的轮询结果比较。缓存实验需要已满足目标模型门槛和断点规则的前缀；短输入的本地 `hit` 不能证明缓存收益。对照实验使用独立前缀或明确记录已有缓存，避免把预热效果误判为亲和收益。

已通过 `go test ./...`、affinity/keypool/proxy 三个包的竞态检查与 `go vet`，以及前端 `npm run build`。本地代理测试覆盖日志状态、请求保真、重定向、解绑及失败 key 排除；未调用真实上游，不预设缓存命中率提升。

## 实现范围与验收

| 文件 | 内容 |
|---|---|
| `internal/affinity/openai_response.go`（新增） | 配置、Responses 资格检查、初始文本前缀提取、工具三元组排序和指纹编码 |
| `internal/affinity/affinity.go` | 注册 `openai-response` 指纹器，调整仅以 Claude 为例的接口注释 |
| `internal/proxy/server.go` | Responses 专属 POST 检查、实际模型解析、HTTP 200 记录条件及失败 key 集合传递 |
| `internal/keypool/provider.go` | 独立的有界 `SelectKeyExcluding`，保留原 `SelectKey` |
| 对应 `*_test.go`、现有 affinity benchmark | 指纹、代理行为、候选排除和资源开销验证 |

无需新增依赖，不修改容器注册、数据库模型、Store 接口或前端。OpenAI 的规范化函数独立实现，不调整已在使用的 Claude 指纹规则。

验收场景如下：

| 类别 | 场景与预期 |
|---|---|
| 范围 | 开关关闭保持原行为；`openai`、其他路由、非 POST 不参与；状态引用、托管 prompt、后台请求跳过 |
| 输入形式 | 字符串 input、单条 user 字符串 content、单个 input_text 块生成相同指纹；允许开头多条 system/developer 消息 |
| 前缀稳定 | 追加 assistant、user、工具结果或内联 reasoning 不改变指纹；后续仍出现 item_reference 则跳过 |
| 内容差异 | 实际模型、提取后的系统文本或首条用户文本变化时改变指纹；指令来源或块边界变化但拼接文本相同时保持指纹 |
| 工具变化 | 增删工具、修改 name / description / type 会改变指纹；Schema、strict、工具顺序和其他设置变化不改变指纹；重复项数量保留 |
| 工具空值 | tools 缺失、null、[] 等价；三元组字段缺失、null、非字符串与空串等价；同名不同描述工具排序确定；非法工具列表跳过 |
| 参数变化 | reasoning、输出格式、tool_choice、缓存保留参数等变化不改变指纹 |
| 规范化 | JSON 对象键顺序、外部空白和等价字符串转义不影响指纹；空文本块忽略；文本内部空白变化仍可区分 |
| 无法提取 | 空输入、缺 user、首条 user 含图片、前置工具结果、未知前缀结构均跳过，不只用公共系统提示词计算 |
| cache key | 不提供也可计算；增加、删除或修改该字段均不改指纹，且不改变其透传值；缓存保留参数或断点同样忽略 |
| 模型与覆盖 | 参数覆盖决定指纹内容；同目标模型的别名可共用，重定向目标变化重新计算；严格模式错误沿用原处理 |
| 存储与分组 | 首次 miss 后 HTTP 200 建立映射，再次 hit；失效/删除/迁移 key 解绑；不同实际分组不互用映射 |
| 失败与重试 | 命中 key 返回可重试错误后解绑；其他 key 成功后记录；轮询首项恰好为失败 key 时仍跳过；全部失败不建立映射 |
| 响应边界 | 不参与 failover 的 404 不建立映射；客户端取消保留已有绑定；HTTP 200 SSE 失败事件仍遵循已声明的限制 |
| TTL 与并发 | 默认及非法配置回退均为 7200 秒；命中不续期，过期后重新轮询；并发首次写只保留一个值，不声称请求合并或条件删除 |
| 请求保真 | 固定分组配置、所选 key 和上游地址，与亲和关闭时比较发送的 body、URL、Header；内容应一致，包括原有参数覆盖、模型重定向和鉴权变换 |
| 回归与性能 | 现有 Claude 测试继续通过；分别测短请求、长工具 schema、较大完整历史的耗时及内存，不将历史全文放入 hash |

## 参考资料

- [1] OpenAI Prompt caching：`https://developers.openai.com/api/docs/guides/prompt-caching`。重点：Cache location、Prompt cache keys、Summary of model differences、Monitor cache performance。
- [2] OpenAI Migrate to Responses：`https://developers.openai.com/api/docs/guides/migrate-to-responses#2-map-messages-to-items`；Create a response：`https://developers.openai.com/api/reference/resources/responses/methods/create`。
- [3] OpenAI Conversation state：`https://developers.openai.com/api/docs/guides/conversation-state`。
- [4] OpenAI API Changelog，2026-05-29 默认保留策略调整：`https://developers.openai.com/api/docs/changelog`。
- [5] OpenAI Codex 官方 `rust-v0.155.1` 源码：`https://github.com/openai/codex/blob/rust-v0.155.1/codex-rs/core/src/client.rs`（`prompt_cache_key`、`build_responses_request`）；`https://github.com/openai/codex/blob/rust-v0.155.1/codex-rs/codex-api/src/common.rs`（`ResponsesApiRequest`）。
- [6] Codex Authentication：`https://learn.chatgpt.com/docs/auth`。API key 与 ChatGPT 登录使用不同的服务及账户规则。
- 本地现有设计：`AFFINITY.md`。通道及运行时行为以本次阅读的 `internal/channel/`、`internal/proxy/`、`internal/affinity/`、`internal/keypool/` 和 `internal/store/` 代码为准。
