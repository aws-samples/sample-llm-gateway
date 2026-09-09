package protocol

import "encoding/json"

type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func parseAnthropicMessageUsage(body []byte) Usage {
	var resp struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return Usage{}
	}
	return Usage{
		Input:      deref(resp.Usage.InputTokens),
		Output:     deref(resp.Usage.OutputTokens),
		CacheRead:  deref(resp.Usage.CacheReadInputTokens),
		CacheWrite: deref(resp.Usage.CacheCreationInputTokens),
		Found:      true,
	}
}

// anthropicStream merges message_start (input side) with message_delta (final output count).
type anthropicStream struct{ u Usage }

func (s *anthropicStream) Feed(_ string, data []byte) {
	if len(data) == 0 {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			u := ev.Message.Usage
			s.u.Input = deref(u.InputTokens)
			s.u.CacheRead = deref(u.CacheReadInputTokens)
			s.u.CacheWrite = deref(u.CacheCreationInputTokens)
			s.u.Output = deref(u.OutputTokens)
			s.u.Found = true
		}
	case "message_delta":
		if ev.Usage != nil {
			u := ev.Usage
			if u.OutputTokens != nil {
				s.u.Output = *u.OutputTokens
			}
			// Newer servers repeat the input-side counters in message_delta; prefer them when present.
			if u.InputTokens != nil {
				s.u.Input = *u.InputTokens
			}
			if u.CacheReadInputTokens != nil {
				s.u.CacheRead = *u.CacheReadInputTokens
			}
			if u.CacheCreationInputTokens != nil {
				s.u.CacheWrite = *u.CacheCreationInputTokens
			}
			s.u.Found = true
		}
	}
}

func (s *anthropicStream) Usage() Usage { return s.u }
