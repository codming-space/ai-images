// Package keypool: Claude 亲和路由的 hash 计算
//
// 移植自 https://github.com/QuantumNous/new-api 的 claude_affinity 中间件。
// 解析 /v1/messages 请求 body，识别是否需要按会话粘到同一上游 key，
// 命中则返回稳定的 SHA256 hex 作为亲和键，未命中返回空字符串。
//
// hash 行为权威实现，跨版本必须稳定，禁止修改字段范围。
package keypool

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// 触发器开关，编译期硬编码，对齐 new-api 默认值
const (
	triggerOnThinking = true
	triggerOnTool     = true
	triggerOnCache    = true
)

// 触发标识，用于日志
const (
	triggerThinking = "thinking"
	triggerTool     = "tool"
	triggerCache    = "cache"
)

type affinityTier string

const (
	tierStrict affinityTier = "strict"
	tierLoose  affinityTier = "loose"
)

// InspectResult 是分析一次 /v1/messages 请求 body 的结果。
// AffinityKey 为空表示无需亲和路由，调用方按原本逻辑选 key。
type InspectResult struct {
	Model       string
	Tier        string
	Triggers    []string
	AffinityKey string
	Reason      string
}

// Inspect 解析请求 body，决定是否需要计算亲和键。该函数不修改 body。
func Inspect(body []byte) InspectResult {
	res := InspectResult{}

	if !gjson.ValidBytes(body) {
		res.Reason = "invalid_json"
		return res
	}

	root := gjson.ParseBytes(body)
	res.Model = root.Get("model").String()

	// 短路：客户端已带 metadata.user_id（Claude Code CLI 会自动加），
	// 交给上游/外层按 user_id 处理，本层不写亲和键。
	if root.Get("metadata.user_id").Exists() {
		res.Reason = "metadata_user_id"
		return res
	}

	triggered := map[string]struct{}{}
	strictHit := false
	looseHit := false

	if triggerOnThinking {
		if t := root.Get("thinking.type"); t.Exists() && t.String() != "disabled" {
			triggered[triggerThinking] = struct{}{}
			strictHit = true
		}
	}
	if triggerOnTool {
		if root.Get("tools.0").Exists() {
			triggered[triggerTool] = struct{}{}
			strictHit = true
		}
	}

	// 遍历 messages 找 content 块级别的触发：
	// - thinking / redacted_thinking 块 → strict（跨上游 signature 失效）
	// - tool_use / tool_result / server_tool_use 块 → strict（跨上游 tool_use_id 失效）
	// - 任意 content 块带 cache_control → strict（被缓存前缀含会变的 message 历史，
	//   不可能跨用户共享，只能按会话粘连）
	if triggerOnThinking || triggerOnTool || triggerOnCache {
		root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
			msg.Get("content").ForEach(func(_, blk gjson.Result) bool {
				switch blk.Get("type").String() {
				case "thinking", "redacted_thinking":
					if triggerOnThinking {
						triggered[triggerThinking] = struct{}{}
						strictHit = true
					}
				case "tool_use", "tool_result", "server_tool_use":
					if triggerOnTool {
						triggered[triggerTool] = struct{}{}
						strictHit = true
					}
				}
				if triggerOnCache && blk.Get("cache_control").Exists() {
					triggered[triggerCache] = struct{}{}
					strictHit = true
				}
				return true
			})
			return true
		})
	}

	// system / tools 顶层的 cache_control 是 LOOSE 场景：被缓存前缀是共享的
	// system 或 tools 段，不同用户应该塌缩到同一渠道复用 cache。
	if triggerOnCache {
		if hasCacheControl(root.Get("system")) {
			triggered[triggerCache] = struct{}{}
			looseHit = true
		}
		if hasCacheControl(root.Get("tools")) {
			triggered[triggerCache] = struct{}{}
			looseHit = true
		}
	}

	if !strictHit && !looseHit {
		return res
	}

	for _, t := range []string{triggerThinking, triggerTool, triggerCache} {
		if _, ok := triggered[t]; ok {
			res.Triggers = append(res.Triggers, t)
		}
	}

	system := normalizedSystem(root.Get("system"))
	tools := canonicalTools(root.Get("tools"))

	// strict 优先：保证多轮 signature / tool_use_id 一致
	if strictHit {
		firstUser := firstUserText(root.Get("messages"))
		if firstUser == "" {
			res.Reason = "no_first_user_text"
			return res
		}
		res.Tier = string(tierStrict)
		res.AffinityKey = computeKey(tierStrict, res.Model, system, tools, firstUser)
		return res
	}

	// loose：仅 cache_control 触发。防退化：system 与 tools 全空时跳过，
	// 否则 hash 退化为 (tier+model) 近常量，全流量会塌到一个 key。
	if system == "" && tools == "" {
		res.Reason = "loose_skipped_empty_prefix"
		return res
	}
	res.Tier = string(tierLoose)
	res.AffinityKey = computeKey(tierLoose, res.Model, system, tools, "")
	return res
}

// hasCacheControl 检查数组型节点是否有任一元素带 cache_control 字段。
// 字符串形式的 system 永远没有 cache_control。
func hasCacheControl(node gjson.Result) bool {
	if !node.Exists() || !node.IsArray() {
		return false
	}
	found := false
	node.ForEach(func(_, item gjson.Result) bool {
		if item.Get("cache_control").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

// normalizedSystem 把 system（字符串或 []block）规约成纯文本。
// cache_control 被剥掉，保证加/移除缓存断点不改 hash。
func normalizedSystem(node gjson.Result) string {
	if !node.Exists() {
		return ""
	}
	if node.Type == gjson.String {
		return node.String()
	}
	if !node.IsArray() {
		return ""
	}
	var sb strings.Builder
	node.ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("type").String() == "text" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(blk.Get("text").String())
		}
		return true
	})
	return sb.String()
}

type toolDef struct {
	name        string
	description string
	schema      string
}

// canonicalTools 把 tools 数组规约成确定性表示：按 name 升序，剔除 cache_control。
func canonicalTools(node gjson.Result) string {
	if !node.Exists() || !node.IsArray() {
		return ""
	}
	var defs []toolDef
	node.ForEach(func(_, t gjson.Result) bool {
		defs = append(defs, toolDef{
			name:        t.Get("name").String(),
			description: t.Get("description").String(),
			schema:      t.Get("input_schema").Raw,
		})
		return true
	})
	sort.Slice(defs, func(i, j int) bool { return defs[i].name < defs[j].name })

	var sb strings.Builder
	for _, d := range defs {
		sb.WriteString(d.name)
		sb.WriteByte(0x1f)
		sb.WriteString(d.description)
		sb.WriteByte(0x1f)
		sb.WriteString(d.schema)
		sb.WriteByte(0x1e)
	}
	return sb.String()
}

// firstUserText 返回 messages 中第一条 role=user 的所有 text 块拼接。
// content 是字符串则直接返回；image / tool_result 等非文本块忽略。
func firstUserText(messages gjson.Result) string {
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	var out string
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			out = content.String()
			return false
		}
		if content.IsArray() {
			var sb strings.Builder
			content.ForEach(func(_, blk gjson.Result) bool {
				if blk.Get("type").String() == "text" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(blk.Get("text").String())
				}
				return true
			})
			out = sb.String()
		}
		return false
	})
	return out
}

// computeKey 用显式分隔符拼接字段后做 SHA256。
// tier 写入 hash 输入，确保 loose 与 first_user 恰好为空的 strict 永不撞 key。
func computeKey(tier affinityTier, model, system, tools, firstUser string) string {
	h := sha256.New()
	writeKV(h, "tier", string(tier))
	writeKV(h, "model", model)
	writeKV(h, "system", system)
	writeKV(h, "tools", tools)
	if tier == tierStrict {
		writeKV(h, "first_user", firstUser)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeKV 用不可打印分隔符（\x00 分隔 key/value，\x01 结尾）写入键值对。
func writeKV(w interface{ Write(p []byte) (int, error) }, key, value string) {
	_, _ = w.Write([]byte(key))
	_, _ = w.Write([]byte{0x00})
	_, _ = w.Write([]byte(value))
	_, _ = w.Write([]byte{0x01})
}
