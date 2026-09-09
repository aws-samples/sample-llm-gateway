package protocol

import "encoding/json"

type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responsesUsage) normalize() Usage {
	read := u.InputTokensDetails.CachedTokens
	write := u.InputTokensDetails.CacheWriteTokens
	input := u.InputTokens - read - write
	if input < 0 {
		input = 0
	}
	return Usage{
		Input:      input,
		Output:     u.OutputTokens,
		CacheRead:  read,
		CacheWrite: write,
		Reasoning:  u.OutputTokensDetails.ReasoningTokens,
		Found:      true,
	}
}

func parseResponsesBodyUsage(body []byte) Usage {
	var resp struct {
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return Usage{}
	}
	return resp.Usage.normalize()
}

// responsesStream reads usage from response.completed / response.incomplete / response.failed.
type responsesStream struct{ u Usage }

func (s *responsesStream) Feed(_ string, data []byte) {
	if len(data) == 0 {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil || ev.Response == nil || ev.Response.Usage == nil {
		return
	}
	switch ev.Type {
	case "response.completed", "response.incomplete", "response.failed":
		s.u = ev.Response.Usage.normalize()
	}
}

func (s *responsesStream) Usage() Usage { return s.u }
