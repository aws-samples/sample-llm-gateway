package protocol

import "encoding/json"

// openAIUsage covers the Chat Completions usage object plus the non-standard cache
// write fields emitted by some gateways/providers.
type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheWriteTokens         int64 `json:"cache_write_tokens"`
}

func (u openAIUsage) normalize() Usage {
	write := firstNonZero(u.PromptTokensDetails.CacheWriteTokens, u.CacheCreationInputTokens, u.CacheWriteTokens)
	read := u.PromptTokensDetails.CachedTokens
	input := u.PromptTokens - read - write
	if input < 0 {
		input = 0
	}
	return Usage{
		Input:      input,
		Output:     u.CompletionTokens,
		CacheRead:  read,
		CacheWrite: write,
		Reasoning:  u.CompletionTokensDetails.ReasoningTokens,
		Found:      true,
	}
}

func parseOpenAIChatUsage(body []byte) Usage {
	var resp struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return Usage{}
	}
	return resp.Usage.normalize()
}

// openAIChatStream picks usage from the final chunk (requires stream_options.include_usage).
type openAIChatStream struct{ u Usage }

func (s *openAIChatStream) Feed(_ string, data []byte) {
	if len(data) == 0 || string(data) == "[DONE]" {
		return
	}
	var chunk struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil || chunk.Usage == nil {
		return
	}
	s.u = chunk.Usage.normalize()
}

func (s *openAIChatStream) Usage() Usage { return s.u }
