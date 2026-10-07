package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/adapter"
	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

// Adapter translates the public Anthropic Messages shape into OpenAI Chat
// Completions. This covers OpenAI-compatible providers such as Ark and Kimi.
type Adapter struct{}

func (Adapter) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

type messageRequest struct {
	Model       string          `json:"model"`
	System      json.RawMessage `json:"system"`
	Messages    []message       `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
}

func (Adapter) Forward(ctx context.Context, upstream provider.Config, credential provider.Credential, _ string, body []byte, _ http.Header) (*http.Response, error) {
	if credential.AuthType == "codex_oauth" {
		return forwardResponses(ctx, upstream, credential, body)
	}
	var incoming messageRequest
	if err := json.Unmarshal(body, &incoming); err != nil {
		return nil, fmt.Errorf("decode Anthropic request for OpenAI: %w", err)
	}
	converted := chatRequest{Model: incoming.Model, MaxTokens: incoming.MaxTokens, Stream: incoming.Stream, Temperature: incoming.Temperature, TopP: incoming.TopP}
	if len(incoming.System) > 0 && string(incoming.System) != "null" {
		converted.Messages = append(converted.Messages, message{Role: "system", Content: textContent(incoming.System)})
	}
	for _, item := range incoming.Messages {
		converted.Messages = append(converted.Messages, message{Role: item.Role, Content: textContentValue(item.Content)})
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI request: %w", err)
	}
	endpoint := strings.TrimRight(upstream.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create OpenAI request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	response, err := (&http.Client{Timeout: adapter.UpstreamTimeout()}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("send OpenAI request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, nil
	}
	if !incoming.Stream {
		return translateResponse(response, incoming.Model)
	}
	return translateStream(response, incoming.Model), nil
}

type responsesRequest struct {
	Model        string    `json:"model"`
	Instructions string    `json:"instructions,omitempty"`
	Input        []message `json:"input"`
	Stream       bool      `json:"stream"`
	Store        bool      `json:"store"`
}

func forwardResponses(ctx context.Context, upstream provider.Config, credential provider.Credential, body []byte) (*http.Response, error) {
	var incoming messageRequest
	if err := json.Unmarshal(body, &incoming); err != nil {
		return nil, fmt.Errorf("decode Anthropic request for Codex Responses: %w", err)
	}
	// ChatGPT-plan OAuth Responses currently requires store=false and streaming;
	// max_output_tokens, temperature, and top_p are not accepted by this flow.
	converted := responsesRequest{Model: incoming.Model, Stream: true, Store: false}
	if len(incoming.System) > 0 && string(incoming.System) != "null" {
		converted.Instructions = textContent(incoming.System)
	}
	for _, item := range incoming.Messages {
		converted.Input = append(converted.Input, message{Role: item.Role, Content: textContentValue(item.Content)})
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("encode Codex Responses request: %w", err)
	}
	endpoint := strings.TrimRight(upstream.BaseURL, "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create Codex Responses request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	req.Header.Set("User-Agent", "ai-gateway-codex-oauth/1.0")
	started := time.Now()
	log.Printf("codex upstream start model=%s endpoint=%s request_bytes=%d", incoming.Model, endpoint, len(encoded))
	response, err := (&http.Client{Timeout: adapter.UpstreamTimeout()}).Do(req)
	if err != nil {
		log.Printf("codex upstream error model=%s after=%s err=%v", incoming.Model, time.Since(started), err)
		return nil, fmt.Errorf("send Codex Responses request: %w", err)
	}
	log.Printf("codex upstream headers model=%s status=%d content_type=%s after=%s", incoming.Model, response.StatusCode, response.Header.Get("Content-Type"), time.Since(started))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, nil
	}
	if !incoming.Stream {
		return translateResponsesStreamResponse(response, incoming.Model)
	}
	return translateResponsesBufferedStream(response, incoming.Model)
}

func translateResponsesStreamResponse(response *http.Response, model string) (*http.Response, error) {
	text, err := readResponsesText(response, model)
	if err != nil {
		return nil, err
	}
	output, _ := json.Marshal(map[string]any{"id": "msg_codex_oauth", "type": "message", "role": "assistant", "model": model, "content": []any{map[string]string{"type": "text", "text": text}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}})
	response.Body = io.NopCloser(bytes.NewReader(output))
	response.ContentLength = int64(len(output))
	response.Header.Set("Content-Type", "application/json")
	return response, nil
}

func translateResponsesBufferedStream(response *http.Response, model string) (*http.Response, error) {
	text, err := readResponsesText(response, model)
	if err != nil {
		return nil, err
	}
	var stream bytes.Buffer
	_ = writeEvent(&stream, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_codex_oauth", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})
	_ = writeEvent(&stream, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	_ = writeEvent(&stream, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}})
	_ = writeEvent(&stream, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	_ = writeEvent(&stream, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}})
	_ = writeEvent(&stream, "message_stop", map[string]string{"type": "message_stop"})
	response.Body = io.NopCloser(bytes.NewReader(stream.Bytes()))
	response.ContentLength = int64(stream.Len())
	response.Header.Set("Content-Type", "text/event-stream")
	log.Printf("codex buffered stream complete model=%s text_bytes=%d response_bytes=%d", model, len(text), stream.Len())
	return response, nil
}

func readResponsesText(response *http.Response, model string) (string, error) {
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var text string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event responsesStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if event.Type != "" {
			log.Printf("codex stream event model=%s type=%s", model, event.Type)
		}
		if event.Type == "response.failed" {
			return "", fmt.Errorf("Codex Responses request failed")
		}
		if event.Type == "response.output_text.delta" {
			text += event.Delta
		}
		if event.Type == "response.output_text.done" && text == "" {
			text = event.Text
		}
		if event.Type == "response.completed" || event.Type == "response.incomplete" {
			if text == "" {
				text = responseOutputText(event.Response.Output)
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read Codex Responses stream: %w", err)
	}
	if text == "" {
		return "", fmt.Errorf("Codex Responses stream contained no assistant text")
	}
	return text, nil
}

func translateResponsesResponse(response *http.Response, model string) (*http.Response, error) {
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read Codex Responses response: %w", err)
	}
	var incoming struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("decode Codex Responses response: %w", err)
	}
	var text string
	for _, item := range incoming.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				text += part.Text
			}
		}
	}
	output, _ := json.Marshal(map[string]any{"id": firstNonEmpty(incoming.ID, "msg_codex_oauth"), "type": "message", "role": "assistant", "model": firstNonEmpty(incoming.Model, model), "content": []any{map[string]string{"type": "text", "text": text}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": incoming.Usage.InputTokens, "output_tokens": incoming.Usage.OutputTokens}})
	response.Body = io.NopCloser(bytes.NewReader(output))
	response.ContentLength = int64(len(output))
	response.Header.Set("Content-Type", "application/json")
	return response, nil
}

func translateResponsesStream(response *http.Response, model string) *http.Response {
	reader, writer := io.Pipe()
	go func() {
		defer response.Body.Close()
		defer writer.Close()
		if err := writeEvent(writer, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_codex_oauth", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}}); err != nil {
			log.Printf("codex stream client_write_error model=%s phase=message_start err=%v", model, err)
			return
		}
		if err := writeEvent(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}}); err != nil {
			log.Printf("codex stream client_write_error model=%s phase=content_block_start err=%v", model, err)
			return
		}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		started := time.Now()
		firstDelta := true
		terminal := "stream_closed"
		var text strings.Builder
		emitText := func(value string) {
			if value == "" {
				return
			}
			text.WriteString(value)
			if firstDelta {
				log.Printf("codex stream first_text model=%s after=%s", model, time.Since(started))
				firstDelta = false
			}
			writeEvent(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": value}})
		}
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var event responsesStreamEvent
			if json.Unmarshal([]byte(data), &event) != nil {
				continue
			}
			if event.Type != "" {
				log.Printf("codex stream event model=%s type=%s", model, event.Type)
			}
			if event.Type == "response.failed" {
				log.Printf("codex stream failed model=%s after=%s", model, time.Since(started))
				writeEvent(writer, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Codex Responses request failed", "details": event.Error}})
				return
			}
			if event.Type == "response.output_text.delta" && event.Delta != "" {
				emitText(event.Delta)
			}
			if event.Type == "response.output_text.done" && event.Text != "" && text.Len() == 0 {
				emitText(event.Text)
			}
			if event.Type == "response.completed" || event.Type == "response.incomplete" {
				if text.Len() == 0 {
					emitText(responseOutputText(event.Response.Output))
				}
				terminal = event.Type
				break
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("codex stream read_error model=%s after=%s err=%v", model, time.Since(started), err)
			writeEvent(writer, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Codex Responses stream read failed"}})
			return
		}
		if text.Len() == 0 {
			log.Printf("codex stream empty model=%s terminal=%s after=%s", model, terminal, time.Since(started))
			writeEvent(writer, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Codex Responses stream contained no assistant text"}})
			return
		}
		log.Printf("codex stream complete model=%s terminal=%s first_text=%t after=%s", model, terminal, !firstDelta, time.Since(started))
		writeEvent(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeEvent(writer, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}})
		writeEvent(writer, "message_stop", map[string]string{"type": "message_stop"})
	}()
	response.Body = reader
	response.Header.Set("Content-Type", "text/event-stream")
	response.ContentLength = -1
	return response
}

type responsesStreamEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Text     string `json:"text"`
	Error    any    `json:"error"`
	Response struct {
		Output []responsesOutputItem `json:"output"`
	} `json:"response"`
}

type responsesOutputItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func responseOutputText(items []responsesOutputItem) string {
	var text strings.Builder
	for _, item := range items {
		for _, part := range item.Content {
			if (part.Type == "output_text" || part.Type == "text") && part.Text != "" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func textContent(raw json.RawMessage) string { return textContentValue(raw) }
func textContentValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(data, &blocks) == nil {
		var parts []string
		for _, block := range blocks {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(data)
}

func translateResponse(response *http.Response, model string) (*http.Response, error) {
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read OpenAI response: %w", err)
	}
	var incoming struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("decode OpenAI response: %w", err)
	}
	text, stop := "", "end_turn"
	if len(incoming.Choices) > 0 {
		text, stop = incoming.Choices[0].Message.Content, mapStopReason(incoming.Choices[0].FinishReason)
	}
	output, _ := json.Marshal(map[string]any{"id": "msg_openai_compat", "type": "message", "role": "assistant", "model": firstNonEmpty(incoming.Model, model), "content": []any{map[string]string{"type": "text", "text": text}}, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": incoming.Usage.PromptTokens, "output_tokens": incoming.Usage.CompletionTokens}})
	response.Body = io.NopCloser(bytes.NewReader(output))
	response.ContentLength = int64(len(output))
	response.Header.Set("Content-Type", "application/json")
	return response, nil
}

func translateStream(response *http.Response, model string) *http.Response {
	reader, writer := io.Pipe()
	go func() {
		defer response.Body.Close()
		defer writer.Close()
		writeEvent(writer, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_openai_compat", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == "" {
				continue
			}
			writeEvent(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": chunk.Choices[0].Delta.Content}})
		}
		writeEvent(writer, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}})
		writeEvent(writer, "message_stop", map[string]string{"type": "message_stop"})
	}()
	response.Body = reader
	response.Header.Set("Content-Type", "text/event-stream")
	response.ContentLength = -1
	return response
}

func writeEvent(writer io.Writer, name string, value any) error {
	data, _ := json.Marshal(value)
	_, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", name, data)
	return err
}
func mapStopReason(reason string) string {
	if reason == "length" {
		return "max_tokens"
	}
	return "end_turn"
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
