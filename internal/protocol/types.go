package protocol

// Request is the provider-neutral representation used between the inbound
// gateway, router, and outbound protocol adapters.
type Request struct {
	Model       string            `json:"model"`
	Messages    []Message         `json:"messages"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Response struct {
	ID      string     `json:"id,omitempty"`
	Model   string     `json:"model"`
	Message Message    `json:"message"`
	Usage   TokenUsage `json:"usage,omitempty"`
}

type TokenUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}
