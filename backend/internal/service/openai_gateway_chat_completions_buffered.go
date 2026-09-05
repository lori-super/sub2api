package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// Opt in by exact mapped model; ordinary accounts and streaming clients retain
// their existing protocol. No guessed context threshold or prompt truncation.
const rawChatBufferedModelsKey = "openai_chat_nonstream_via_stream_models"

func shouldBufferRawChatStream(account *Account, model string, clientStream bool) bool {
	if clientStream || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return false
	}
	switch models := account.Extra[rawChatBufferedModelsKey].(type) {
	case []any:
		for _, candidate := range models {
			if value, ok := candidate.(string); ok && value == model {
				return true
			}
		}
	case []string:
		for _, candidate := range models {
			if candidate == model {
				return true
			}
		}
	}
	return false
}

// Read a bounded stream before writing anything to the client. Reuse the normal
// JSON path afterwards so response model observation and usage accounting remain
// identical to a native non-stream response.
func (s *OpenAIGatewayService) bufferRawChatStreamResponse(c *gin.Context, response *http.Response, expectedChoices int) (*http.Response, error) {
	limit := resolveUpstreamResponseReadLimit(s.cfg)
	limited := &io.LimitedReader{R: response.Body, N: limit + 1}
	scanner := s.newUpstreamSSEScanner(limited)
	root := map[string]any{"object": "chat.completion"}
	choices := map[int]map[string]any{}
	done := false
	var upstreamErrorBody []byte
	var eventLines []string
	fail := func(message string) (*http.Response, error) {
		status := http.StatusBadGateway
		errorType := "upstream_error"
		if isOpenAIContextWindowError(message, upstreamErrorBody) {
			status = http.StatusBadRequest
			errorType = "invalid_request_error"
		}
		setOpsUpstreamError(c, status, message, "")
		writeChatCompletionsError(c, status, errorType, message)
		return nil, fmt.Errorf("buffer upstream chat stream: %s", message)
	}
	consume := func(payload string) error {
		if strings.TrimSpace(payload) == "[DONE]" {
			done = true
			return nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || chunk == nil {
			return fmt.Errorf("invalid upstream chat stream event")
		}
		if upstreamError, exists := chunk["error"]; exists && upstreamError != nil {
			upstreamErrorBody = []byte(payload)
			message := "Upstream chat stream returned an error"
			if detail, ok := upstreamError.(map[string]any); ok {
				if text, ok := detail["message"].(string); ok && text != "" {
					message = sanitizeUpstreamErrorMessage(text)
				}
			}
			return fmt.Errorf("%s", message)
		}
		for key, value := range chunk {
			if key == "choices" || key == "object" || value == nil {
				continue
			}
			if key == "id" || key == "model" {
				text, ok := value.(string)
				if !ok {
					return fmt.Errorf("invalid upstream chat stream identity")
				}
				if previous, ok := root[key].(string); ok && previous != text {
					return fmt.Errorf("inconsistent upstream chat stream identity")
				}
			}
			root[key] = value
		}
		rawChoices, ok := chunk["choices"].([]any)
		if !ok {
			return fmt.Errorf("invalid upstream chat stream choices")
		}
		for _, rawChoice := range rawChoices {
			item, ok := rawChoice.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid upstream chat choice")
			}
			indexValue, ok := item["index"].(float64)
			if !ok || indexValue < 0 || indexValue >= float64(expectedChoices) || indexValue != float64(int(indexValue)) {
				return fmt.Errorf("invalid upstream chat choice index")
			}
			index := int(indexValue)
			choice := choices[index]
			if choice == nil {
				choice = map[string]any{"index": index, "message": map[string]any{"role": "assistant", "content": nil}, "finish_reason": nil}
				choices[index] = choice
			}
			if rawDelta := item["delta"]; rawDelta != nil {
				delta, ok := rawDelta.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid upstream chat delta")
				}
				if choice["finish_reason"] != nil && len(delta) > 0 {
					return fmt.Errorf("upstream chat choice changed after completion")
				}
				message, _ := choice["message"].(map[string]any)
				if err := mergeBufferedChatDelta(message, delta); err != nil {
					return err
				}
			}
			if reason, ok := item["finish_reason"].(string); ok && reason != "" {
				choice["finish_reason"] = reason
			}
			if logprobs, ok := item["logprobs"].(map[string]any); ok {
				current, _ := choice["logprobs"].(map[string]any)
				if current == nil {
					current = map[string]any{}
					choice["logprobs"] = current
				}
				for field, entries := range logprobs {
					if entries == nil {
						continue
					}
					items, ok := entries.([]any)
					if !ok {
						return fmt.Errorf("invalid upstream chat logprobs")
					}
					previous, _ := current[field].([]any)
					current[field] = append(previous, items...)
				}
			}
		}
		return nil
	}
	flush := func() error {
		if len(eventLines) == 0 {
			return nil
		}
		payload := strings.Join(eventLines, "\n")
		eventLines = nil
		return consume(payload)
	}
	for scanner.Scan() {
		if limited.N <= 0 {
			return fail("Upstream response too large")
		}
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return fail(err.Error())
			}
			if done {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			eventLines = append(eventLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if limited.N <= 0 {
		return fail("Upstream response too large")
	}
	if err := scanner.Err(); err != nil {
		return fail("Failed to read complete upstream chat stream")
	}
	if !done {
		if err := flush(); err != nil {
			return fail(err.Error())
		}
	}
	if !done || len(choices) != expectedChoices {
		return fail("Upstream chat stream ended before completion")
	}
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return fail("Upstream chat stream omitted usage")
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens"} {
		value, ok := usage[key].(float64)
		if !ok || value < 0 || value != float64(int64(value)) {
			return fail("Upstream chat stream returned invalid usage")
		}
	}
	ordered := make([]any, 0, len(choices))
	for index := 0; index < expectedChoices; index++ {
		choice := choices[index]
		if choice == nil || choice["finish_reason"] == nil {
			return fail("Upstream chat stream omitted a choice finish reason")
		}
		message, _ := choice["message"].(map[string]any)
		if tools, ok := message["tool_calls"].([]any); ok {
			sort.Slice(tools, func(i, j int) bool {
				left, _ := tools[i].(map[string]any)
				right, _ := tools[j].(map[string]any)
				leftIndex, _ := left["index"].(float64)
				rightIndex, _ := right["index"].(float64)
				return leftIndex < rightIndex
			})
			for _, tool := range tools {
				item, _ := tool.(map[string]any)
				delete(item, "index")
			}
		}
		ordered = append(ordered, choice)
	}
	root["choices"] = ordered
	finalizeBufferedChatStrings(root)
	body, err := json.Marshal(root)
	if err != nil {
		return fail("Failed to encode buffered chat response")
	}
	if int64(len(body)) > limit {
		return fail("Upstream response too large")
	}
	buffered := *response
	buffered.Header = response.Header.Clone()
	buffered.Header.Set("Content-Type", "application/json")
	for _, key := range []string{"Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		buffered.Header.Del(key)
	}
	buffered.Body = io.NopCloser(bytes.NewReader(body))
	buffered.ContentLength = int64(len(body))
	return &buffered, nil
}

// Text and function argument fragments append; tool calls merge by their stream
// index so multiple calls and fragmented identities are not lost.
func mergeBufferedChatDelta(target, delta map[string]any) error {
	for key, value := range delta {
		if value == nil {
			continue
		}
		switch key {
		case "content", "reasoning_content", "refusal", "arguments", "name", "id", "role", "type":
			if _, ok := value.(string); !ok {
				return fmt.Errorf("invalid upstream chat text field")
			}
		}
		if key == "tool_calls" {
			incoming, ok := value.([]any)
			if !ok {
				return fmt.Errorf("invalid upstream tool calls")
			}
			tools, _ := target[key].([]any)
			for _, entry := range incoming {
				tool, ok := entry.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid upstream tool call")
				}
				index, ok := tool["index"].(float64)
				if !ok || index < 0 || index != float64(int(index)) {
					return fmt.Errorf("invalid upstream tool call index")
				}
				var current map[string]any
				for _, existing := range tools {
					candidate, _ := existing.(map[string]any)
					if candidate["index"] == index {
						current = candidate
						break
					}
				}
				if current == nil {
					current = map[string]any{"index": index}
					tools = append(tools, current)
				}
				if err := mergeBufferedChatDelta(current, tool); err != nil {
					return err
				}
			}
			target[key] = tools
			continue
		}
		switch fragment := value.(type) {
		case string:
			if key == "role" || key == "type" {
				if fragment != "" {
					target[key] = fragment
				}
				continue
			}
			builder, ok := target[key].(*strings.Builder)
			if !ok {
				builder = &strings.Builder{}
				if previous, ok := target[key].(string); ok {
					_, _ = builder.WriteString(previous)
				}
				target[key] = builder
			}
			_, _ = builder.WriteString(fragment)
		case map[string]any:
			current, _ := target[key].(map[string]any)
			if current == nil {
				current = map[string]any{}
				target[key] = current
			}
			if err := mergeBufferedChatDelta(current, fragment); err != nil {
				return err
			}
		case []any:
			current, _ := target[key].([]any)
			target[key] = append(current, fragment...)
		default:
			target[key] = value
		}
	}
	return nil
}

func finalizeBufferedChatStrings(value any) {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if builder, ok := child.(*strings.Builder); ok {
				item[key] = builder.String()
			} else {
				finalizeBufferedChatStrings(child)
			}
		}
	case []any:
		for _, child := range item {
			finalizeBufferedChatStrings(child)
		}
	}
}
