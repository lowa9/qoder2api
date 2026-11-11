package bridge

import (
	"encoding/json"
	"os"
	"strings"

	"qoder2api/logger"
)

func BuildQoderMessages(templateMsgs []interface{}, incoming []interface{}, prompt string, toolsEnabled bool) []interface{} {
	var rebuilt []interface{}

	hasIncomingSystem := false
	for _, m := range incoming {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
			hasIncomingSystem = true
			break
		}
	}
	if !hasIncomingSystem && templateMsgs != nil {
		for _, m := range templateMsgs {
			if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
				rebuilt = append(rebuilt, DeepCopyMap(mm))
			}
		}
	}

	for _, m := range incoming {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// Handle multi-tool-result: a single Claude user message may contain multiple tool_results
		if role, _ := mm["role"].(string); role == "user" {
			if contentArr, ok := mm["content"].([]interface{}); ok && len(contentArr) > 0 {
				if first, ok := contentArr[0].(map[string]interface{}); ok && first["type"] == "tool_result" {
					for _, block := range contentArr {
						b, _ := block.(map[string]interface{})
						if b == nil || b["type"] != "tool_result" {
							continue
						}
						toolCallId, _ := b["tool_use_id"].(string)
						resultContent := ""
						switch c := b["content"].(type) {
						case string:
							resultContent = c
						case []interface{}:
							for _, part := range c {
								if pm, ok := part.(map[string]interface{}); ok {
									if t, ok := pm["text"].(string); ok {
										resultContent += t
									}
								}
							}
						}
						rebuilt = append(rebuilt, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolCallId,
							"content":      resultContent,
						})
					}
					continue
				}
			}
		}
		converted := ConvertIncomingMessage(mm, toolsEnabled)
		if converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}

	if len(rebuilt) == 0 && prompt != "" {
		rebuilt = append(rebuilt, BuildUserMessage(prompt))
	}
	return rebuilt
}

func ConvertIncomingMessage(msg map[string]interface{}, toolsEnabled bool) map[string]interface{} {
	role, _ := msg["role"].(string)
	content := msg["content"]

	// Handle Claude Messages API content blocks (array of typed blocks)
	if contentArr, ok := content.([]interface{}); ok && len(contentArr) > 0 {
		firstBlock, _ := contentArr[0].(map[string]interface{})
		blockType, _ := firstBlock["type"].(string)

		// assistant message with tool_use blocks
		if role == "assistant" && HasBlockType(contentArr, "tool_use") {
			out := map[string]interface{}{"role": "assistant", "content": ""}
			var toolCalls []interface{}
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil {
					continue
				}
				switch b["type"] {
				case "tool_use":
					inputJSON, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b["id"],
						"type": "function",
						"function": map[string]interface{}{
							"name":      b["name"],
							"arguments": string(inputJSON),
						},
					})
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts != "" {
				out["content"] = textParts
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// user message with tool_result blocks
		if role == "user" && blockType == "tool_result" {
			var results []map[string]interface{}
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil || b["type"] != "tool_result" {
					continue
				}
				toolCallId, _ := b["tool_use_id"].(string)
				resultContent := ""
				switch c := b["content"].(type) {
				case string:
					resultContent = c
				case []interface{}:
					for _, part := range c {
						if pm, ok := part.(map[string]interface{}); ok {
							if t, ok := pm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
				results = append(results, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolCallId,
					"content":      resultContent,
				})
			}
			if len(results) == 1 {
				return results[0]
			}
			// Multiple tool results: return first, caller should handle multi
			// Actually we need to return all - use a special marker
			if len(results) > 0 {
				return results[0]
			}
			return nil
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts == "" {
				return nil
			}
			return BuildStructuredMessage(role, textParts)
		}
	}

	text := NormalizeMessageContent(msg)
	images := ExtractMessageImages(msg)

	if role == "assistant" && toolsEnabled {
		if tc, ok := msg["tool_calls"]; ok {
			out := BuildStructuredMessage("assistant", text)
			out["tool_calls"] = tc
			return out
		}
	}

	if role == "tool" {
		out := BuildStructuredMessage("tool", text)
		if name, ok := msg["name"].(string); ok {
			out["name"] = name
		}
		if tcid, ok := msg["tool_call_id"].(string); ok {
			out["tool_call_id"] = tcid
		}
		return out
	}

	if text == "" && len(images) == 0 {
		return nil
	}

	if role == "user" {
		return BuildUserMessageWithImages(text, images)
	}
	return BuildStructuredMessage(role, text)
}

func HasBlockType(blocks []interface{}, blockType string) bool {
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == blockType {
				return true
			}
		}
	}
	return false
}

// BuildUserMessage 构造纯文本 user 消息（保留原有签名，内部转发到多模态版本）。
func BuildUserMessage(text string) map[string]interface{} {
	return BuildUserMessageWithImages(text, nil)
}

// BuildUserMessageWithImages 构造 Qoder 上游的 user 消息。
//
// Qoder 的 contents 数组接受 OpenAI 风格的 content parts：图片在前
// ({type:image_url, image_url:{url}})，文本在后 ({type:text, text})。
// 图片 URL 支持 data: base64 与 http(s) 两种形态，与官方客户端一致。
func BuildUserMessageWithImages(text string, images []string) map[string]interface{} {
	contents := make([]interface{}, 0, len(images)+1)
	for _, url := range images {
		contents = append(contents, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": url},
		})
	}
	if text != "" || len(contents) == 0 {
		contents = append(contents, map[string]interface{}{"type": "text", "text": text})
	}
	return map[string]interface{}{
		"role":                        "user",
		"content":                     "",
		"contents":                    contents,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

// ExtractMessageImages 从消息的 content 数组中提取图片 URL。
// 支持三种形态：
//   - OpenAI Chat Completions: {type:image_url, image_url:{url}}
//   - OpenAI Responses API:    {type:input_image, image_url:"..."}
//   - Claude Messages API:     {type:image, source:{type:base64|url, media_type, data|url}}
//
// 空 URL 与非图片 URL（既不是 data: 也不是 http(s)）会被丢弃。
func ExtractMessageImages(msg map[string]interface{}) []string {
	arr, ok := msg["content"].([]interface{})
	if !ok {
		// 兼容 Qoder 风格：图片可能直接放在 contents 数组里（参考 EchoPing07）
		arr, ok = msg["contents"].([]interface{})
		if !ok {
			return nil
		}
	}
	var images []string
	for _, block := range arr {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := b["type"].(string)
		switch t {
		case "image_url", "input_image":
			if url := imageURLFromPart(b); url != "" && looksLikeImageURL(url) {
				images = append(images, url)
			}
		case "image":
			if url := claudeImageToURL(b); url != "" {
				images = append(images, url)
			}
		}
	}
	return images
}

// imageURLFromPart 兼容 {image_url:{url}}、{image_url:"..."} 与 {url:"..."} 三种写法。
func imageURLFromPart(part map[string]interface{}) string {
	switch v := part["image_url"].(type) {
	case string:
		return v
	case map[string]interface{}:
		if s, ok := v["url"].(string); ok {
			return s
		}
	}
	if s, ok := part["url"].(string); ok {
		return s
	}
	return ""
}

// claudeImageToURL 把 Claude 的 image block 转成 data:/http URL。
func claudeImageToURL(block map[string]interface{}) string {
	src, ok := block["source"].(map[string]interface{})
	if !ok {
		return ""
	}
	switch st, _ := src["type"].(string); st {
	case "base64":
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if mt != "" && data != "" {
			return "data:" + mt + ";base64," + data
		}
	case "url":
		if u, ok := src["url"].(string); ok && looksLikeImageURL(u) {
			return u
		}
	}
	return ""
}

// looksLikeImageURL 只放行 data: 与 http(s) 两类，避免把垃圾塞给上游。
func looksLikeImageURL(url string) bool {
	return strings.HasPrefix(url, "data:") ||
		strings.HasPrefix(url, "http://") ||
		strings.HasPrefix(url, "https://")
}

func BuildStructuredMessage(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role":                        role,
		"content":                     text,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BlankResponseMeta() map[string]interface{} {
	return map[string]interface{}{
		"id": "",
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]interface{}{"cached_tokens": 0},
		},
	}
}

func NormalizeMessageContent(msg map[string]interface{}) string {
	c := msg["content"]
	if s, ok := c.(string); ok {
		return s
	}
	if c == nil {
		return ""
	}
	arr, ok := c.([]interface{})
	if !ok {
		data, _ := json.Marshal(c)
		return string(data)
	}
	var sb string
	for _, block := range arr {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if sb != "" {
				sb += "\n\n"
			}
			sb += t
		}
	}
	return sb
}

// isTaskNotificationReminder 判断一个 <system-reminder> 块是否是后台任务完成
// 通知。Claude Code 会把这类通知渲染成 system 角色。
func isTaskNotificationReminder(block string) bool {
	low := strings.ToLower(block)
	return strings.Contains(low, "<task-notification>") ||
		strings.Contains(low, "system notification - not user input")
}

// relocateTaskNotifications 把 system 消息中的后台任务通知块移到消息末尾。
//
// Claude Code 会把后台任务完成通知渲染成 system 角色；cc-switch 等代理会把
// 所有 system 块合并进第一条 system 消息。通知一变，system 前缀和整个
// prompt cache 就失效（实测 77 万 token 的会话只剩 5.4 万 token 命中）。
// 移到末尾后，稳定 system + 历史对话仍构成相同前缀，缓存可以继续命中；
// 通知仍保持 system 角色（上游实测会读取末尾的 system 消息）。
func relocateTaskNotifications(messages []interface{}) []interface{} {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("QODER2API_RELOCATE_NOTIFICATIONS")), "off") {
		return messages
	}
	var moved []string
	changed := false
	out := make([]interface{}, 0, len(messages)+1)
	for _, it := range messages {
		m, ok := it.(map[string]interface{})
		if !ok {
			out = append(out, it)
			continue
		}
		if role, _ := m["role"].(string); role != "system" {
			out = append(out, it)
			continue
		}
		content, ok := m["content"].(string)
		if !ok {
			out = append(out, it)
			continue
		}
		kept := content
		movedHere := 0
		for _, block := range systemReminderPattern.FindAllString(content, -1) {
			if !isTaskNotificationReminder(block) {
				continue
			}
			moved = append(moved, block)
			kept = strings.Replace(kept, block, "", 1)
			movedHere++
		}
		if movedHere == 0 {
			out = append(out, it)
			continue
		}
		changed = true
		kept = strings.TrimSpace(kept)
		if kept == "" {
			continue
		}
		nm := make(map[string]interface{}, len(m))
		for k, v := range m {
			nm[k] = v
		}
		nm["content"] = kept
		out = append(out, nm)
	}
	if !changed {
		return messages
	}
	logger.Info("[relocate] moved %d task notification block(s) from the system prefix to the message tail", len(moved))
	if len(moved) > 0 {
		out = append(out, map[string]interface{}{
			"role":    "system",
			"content": strings.Join(moved, "\n\n"),
		})
	}
	return out
}

// stripClientMetaFromMessages 删除 system 消息里的客户端传输层元数据行。
//
// Claude Code 会注入 `x-anthropic-billing-header: ... cch=xxxxx;`，其它客户端
// （omp 等）可能用别的 x-* 头。这些行不是模型指令，且带每次都变的字段。
// Qwen/Kimi 的上游前缀缓存严格按前缀匹配：实测同一个 ~20k 前缀仅 cch 变化
// 就让命中率从 100% 掉到 0%（DeepSeek 不受影响）。Qoder 不校验这些头。
//
// 用通用规则（见 clientMetaLinePattern）而非逐字段硬编码，换客户端或上游
// 新增字段都不必再改代码。剥离了什么会打一条 info 日志便于发现新字段。
func stripClientMetaFromMessages(messages []interface{}) []interface{} {
	out := make([]interface{}, 0, len(messages))
	changed := false
	var removed []string
	for _, it := range messages {
		m, ok := it.(map[string]interface{})
		if !ok {
			out = append(out, it)
			continue
		}
		if role, _ := m["role"].(string); role != "system" {
			out = append(out, it)
			continue
		}
		content, ok := m["content"].(string)
		if !ok {
			out = append(out, it)
			continue
		}
		for _, line := range clientMetaLinePattern.FindAllString(content, -1) {
			removed = append(removed, strings.TrimSpace(line))
		}
		stripped := stripClientMarkers(content)
		if stripped == strings.TrimSpace(content) {
			out = append(out, it)
			continue
		}
		changed = true
		if stripped == "" {
			continue
		}
		nm := make(map[string]interface{}, len(m))
		for k, v := range m {
			nm[k] = v
		}
		nm["content"] = stripped
		out = append(out, nm)
	}
	if !changed {
		return messages
	}
	for _, line := range removed {
		logger.Info("[clientmeta] stripped from upstream system: %s", truncateRunesForLog(line, 200))
	}
	return out
}
