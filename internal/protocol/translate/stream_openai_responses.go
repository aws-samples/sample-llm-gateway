package translate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/protocol"
)

// ======================================================================================
// OpenAI Responses SSE  →  IR events
// ======================================================================================
//
// Real event order (recorded from Bedrock, see testdata/fixtures/codex/01-*.sse). Bedrock emits
// data-only frames (no "event:" line); the type is in data.type. Per output item:
//
//	response.created / response.in_progress
//	response.output_item.added{output_index,item{type:message|function_call|reasoning,id,call_id,name}}
//	response.content_part.added → response.output_text.delta{delta}* → response.output_text.done → response.content_part.done
//	response.function_call_arguments.delta{item_id,delta}* → response.function_call_arguments.done
//	response.reasoning_summary_text.delta* (when reasoning is exposed)
//	response.output_item.done{item{...complete...}}
//	response.completed{response{status,output[],usage}}  |  response.incomplete / response.failed
type responsesStreamIn struct {
	started bool
	id      string
	model   string
	items   map[int]*respItemState // output_index → state
	byID    map[string]int         // item_id → output_index
	hasTool bool
	stopped bool
}

type respItemState struct {
	kind   ContentKind
	callID string
	name   string
	textOn bool
}

func newResponsesStreamIn() *responsesStreamIn {
	return &responsesStreamIn{items: map[int]*respItemState{}, byID: map[string]int{}}
}

type respEvent struct {
	Type        string         `json:"type"`
	OutputIndex int            `json:"output_index"`
	ItemID      string         `json:"item_id"`
	Delta       string         `json:"delta"`
	Item        *responsesItem `json:"item,omitempty"`
	Response    *struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response,omitempty"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (s *responsesStreamIn) ToIR(_ string, data []byte) ([]StreamEvent, error) {
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil, nil
	}
	var ev respEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("openai responses stream: %w", err)
	}
	switch ev.Type {
	case "response.created":
		if s.started {
			return nil, nil
		}
		s.started = true
		if ev.Response != nil {
			s.id, s.model = ev.Response.ID, ev.Response.Model
		}
		return []StreamEvent{{Kind: EventMessageStart, ID: s.id, Model: s.model}}, nil

	case "response.output_item.added":
		if ev.Item == nil {
			return nil, nil
		}
		st := &respItemState{callID: ev.Item.CallID, name: ev.Item.Name}
		s.items[ev.OutputIndex] = st
		if ev.Item.ID != "" {
			s.byID[ev.Item.ID] = ev.OutputIndex
		}
		switch ev.Item.Type {
		case "message":
			st.kind = KindText
		case "function_call":
			st.kind = KindToolUse
			s.hasTool = true
			return []StreamEvent{{Kind: EventToolUseStart, Index: ev.OutputIndex, ToolCallID: st.callID, ToolName: st.name}}, nil
		case "reasoning":
			st.kind = KindThinking
		}
		return nil, nil

	case "response.output_text.delta":
		idx := s.indexFor(ev)
		return []StreamEvent{{Kind: EventTextDelta, Index: idx, Text: ev.Delta}}, nil

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		idx := s.indexFor(ev)
		return []StreamEvent{{Kind: EventThinkingDelta, Index: idx, Text: ev.Delta}}, nil

	case "response.function_call_arguments.delta":
		idx := s.indexFor(ev)
		st := s.items[idx]
		out := StreamEvent{Kind: EventToolInputDelta, Index: idx, ToolInputDelta: ev.Delta}
		if st != nil {
			out.ToolCallID, out.ToolName = st.callID, st.name
		}
		return []StreamEvent{out}, nil

	case "response.output_item.done":
		idx := s.indexFor(ev)
		st := s.items[idx]
		if st == nil || ev.Item == nil {
			return nil, nil
		}
		switch st.kind {
		case KindToolUse:
			// Fill in call_id/name if the "added" event lacked them.
			if st.callID == "" {
				st.callID = ev.Item.CallID
			}
			if st.name == "" {
				st.name = ev.Item.Name
			}
			return []StreamEvent{{Kind: EventToolUseStop, Index: idx, ToolCallID: st.callID, ToolName: st.name}}, nil
		case KindThinking:
			// The completed reasoning item is the opaque round-trip payload.
			raw, _ := json.Marshal(ev.Item)
			return []StreamEvent{{Kind: EventThinkingDone, Index: idx, ThinkingRaw: raw, Text: reasoningSummaryText(ev.Item.Summary)}}, nil
		}
		return nil, nil

	case "response.completed", "response.incomplete", "response.failed":
		if s.stopped {
			return nil, nil
		}
		s.stopped = true
		var stop StopReason = StopEndTurn
		if ev.Response != nil {
			stop = responsesStatusToIR(ev.Response.Status, ev.Response.IncompleteDetails, s.hasTool)
			if ev.Type == "response.failed" && ev.Response.Error != nil {
				return []StreamEvent{{Kind: EventError, Err: ev.Response.Error.Code + ": " + ev.Response.Error.Message}}, nil
			}
		} else if s.hasTool {
			stop = StopToolUse
		}
		usage := protocol.ParseUsage(protocol.OpenAIResponses, wrapResponseForUsage(data))
		return []StreamEvent{{Kind: EventMessageStop, StopReason: stop, Usage: usage}}, nil

	case "error":
		msg := "upstream error"
		if ev.Error != nil {
			msg = ev.Error.Code + ": " + ev.Error.Message
		}
		return []StreamEvent{{Kind: EventError, Err: msg}}, nil
	}
	// response.in_progress, content_part.*, output_text.done, function_call_arguments.done, etc.
	return nil, nil
}

func (s *responsesStreamIn) indexFor(ev respEvent) int {
	if ev.ItemID != "" {
		if idx, ok := s.byID[ev.ItemID]; ok {
			return idx
		}
	}
	return ev.OutputIndex
}

// wrapResponseForUsage feeds the terminal event's response.usage to protocol.ParseUsage, which
// expects the usage at body.usage.
func wrapResponseForUsage(data []byte) []byte {
	var ev struct {
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil || len(ev.Response.Usage) == 0 {
		return []byte("{}")
	}
	return append(append([]byte(`{"usage":`), ev.Response.Usage...), '}')
}

// ======================================================================================
// IR events  →  OpenAI Responses SSE
// ======================================================================================
//
// Synthesizes the event family Codex consumes. Frames are data-only with data.type (what Bedrock
// and OpenAI emit); sequence_number increments per frame.
type responsesStreamOut struct {
	id, model string
	created   int64
	seq       int
	nextOut   int
	open      *respOutItem // currently open output item (message or function_call)
	srcToItem map[int]*respOutItem
	output    []any // completed items for response.completed
	textAcc   string
	argsAcc   map[string]string
}

type respOutItem struct {
	kind   ContentKind
	outIdx int
	itemID string
	callID string
	name   string
	text   string
	args   string
	srcIdx int // source Index this item maps from (for cleaning up srcToItem when closed)
}

func newResponsesStreamOut() *responsesStreamOut {
	return &responsesStreamOut{srcToItem: map[int]*respOutItem{}, argsAcc: map[string]string{}}
}

func (s *responsesStreamOut) frame(v map[string]any) []byte {
	v["sequence_number"] = s.seq
	s.seq++
	b, _ := json.Marshal(v)
	return []byte("data: " + string(b) + "\n\n")
}

func (s *responsesStreamOut) responseObj(status string, usage any, incomplete any) map[string]any {
	out := s.output
	if out == nil {
		out = []any{}
	}
	return map[string]any{
		"id": s.id, "object": "response", "created_at": s.created, "status": status, "model": s.model,
		"output": out, "usage": usage, "error": nil, "incomplete_details": incomplete,
	}
}

func (s *responsesStreamOut) FromIR(ev StreamEvent) ([]byte, error) {
	var out []byte
	switch ev.Kind {
	case EventMessageStart:
		s.id, s.model, s.created = ev.ID, ev.Model, time.Now().Unix()
		if s.id == "" {
			s.id = "resp_" + randomID()
		}
		out = append(out, s.frame(map[string]any{"type": "response.created", "response": s.responseObj("in_progress", nil, nil)})...)
		out = append(out, s.frame(map[string]any{"type": "response.in_progress", "response": s.responseObj("in_progress", nil, nil)})...)
		return out, nil

	case EventTextDelta:
		if s.open == nil || s.open.kind != KindText {
			out = append(out, s.closeOpen()...)
			it := &respOutItem{kind: KindText, outIdx: s.nextOut, itemID: "msg_" + randomID()}
			s.nextOut++
			s.open = it
			out = append(out, s.frame(map[string]any{"type": "response.output_item.added", "output_index": it.outIdx,
				"item": map[string]any{"type": "message", "id": it.itemID, "status": "in_progress", "role": "assistant", "content": []any{}}})...)
			out = append(out, s.frame(map[string]any{"type": "response.content_part.added", "output_index": it.outIdx, "content_index": 0, "item_id": it.itemID,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})...)
		}
		s.open.text += ev.Text
		return append(out, s.frame(map[string]any{"type": "response.output_text.delta", "output_index": s.open.outIdx, "content_index": 0, "item_id": s.open.itemID, "delta": ev.Text})...), nil

	case EventThinkingDelta:
		if s.open == nil || s.open.kind != KindThinking {
			out = append(out, s.closeOpen()...)
			it := &respOutItem{kind: KindThinking, outIdx: s.nextOut, itemID: "rs_" + randomID()}
			s.nextOut++
			s.open = it
			out = append(out, s.frame(map[string]any{"type": "response.output_item.added", "output_index": it.outIdx,
				"item": map[string]any{"type": "reasoning", "id": it.itemID, "summary": []any{}}})...)
		}
		s.open.text += ev.Text
		return append(out, s.frame(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": s.open.outIdx, "summary_index": 0, "item_id": s.open.itemID, "delta": ev.Text})...), nil

	case EventThinkingDone:
		// A genuine Responses reasoning item (opaque) can be re-emitted whole; Anthropic
		// signatures cannot be represented — close whatever summary text we streamed.
		if len(ev.ThinkingRaw) > 0 {
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(ev.ThinkingRaw, &probe) == nil && probe.Type == "reasoning" {
				out = append(out, s.closeOpen()...)
				item := json.RawMessage(ev.ThinkingRaw) // verbatim; map[string]any would float64/reorder
				idx := s.nextOut
				s.nextOut++
				out = append(out, s.frame(map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})...)
				out = append(out, s.frame(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": item})...)
				s.output = append(s.output, item)
				return out, nil
			}
		}
		if s.open != nil && s.open.kind == KindThinking {
			out = append(out, s.closeOpen()...)
		}
		return out, nil

	case EventToolUseStart:
		out = append(out, s.closeOpen()...)
		callID := ev.ToolCallID
		if callID == "" {
			callID = "call_" + randomID()
		}
		it := &respOutItem{kind: KindToolUse, outIdx: s.nextOut, itemID: "fc_" + randomID(), callID: callID, name: ev.ToolName, srcIdx: ev.Index}
		s.nextOut++
		s.open = it
		s.srcToItem[ev.Index] = it
		return append(out, s.frame(map[string]any{"type": "response.output_item.added", "output_index": it.outIdx,
			"item": map[string]any{"type": "function_call", "id": it.itemID, "status": "in_progress", "call_id": it.callID, "name": it.name, "arguments": ""}})...), nil

	case EventToolInputDelta:
		it, ok := s.srcToItem[ev.Index]
		if !ok {
			b, _ := s.FromIR(StreamEvent{Kind: EventToolUseStart, Index: ev.Index, ToolCallID: ev.ToolCallID, ToolName: ev.ToolName})
			out = append(out, b...)
			it = s.srcToItem[ev.Index]
		}
		if ev.ToolInputDelta == "" {
			return out, nil
		}
		it.args += ev.ToolInputDelta
		return append(out, s.frame(map[string]any{"type": "response.function_call_arguments.delta", "output_index": it.outIdx, "item_id": it.itemID, "delta": ev.ToolInputDelta})...), nil

	case EventToolUseStop:
		it, ok := s.srcToItem[ev.Index]
		if !ok {
			return nil, nil
		}
		delete(s.srcToItem, ev.Index)
		if s.open == it {
			s.open = nil
		}
		return s.closeTool(it), nil

	case EventMessageStop:
		out = append(out, s.closeOpen()...)
		status, incomplete := responsesStatusFromIR(ev.StopReason)
		typ := "response.completed"
		if status == "incomplete" {
			typ = "response.incomplete"
		}
		return append(out, s.frame(map[string]any{"type": typ, "response": s.responseObj(status, responsesUsageFromIR(ev.Usage), incomplete)})...), nil

	case EventError:
		out = append(out, s.closeOpen()...)
		resp := s.responseObj("failed", nil, nil)
		resp["error"] = map[string]any{"code": "server_error", "message": ev.Err}
		return append(out, s.frame(map[string]any{"type": "response.failed", "response": resp})...), nil
	}
	return nil, nil
}

func (s *responsesStreamOut) closeOpen() []byte {
	if s.open == nil {
		return nil
	}
	it := s.open
	s.open = nil
	switch it.kind {
	case KindText:
		item := map[string]any{"type": "message", "id": it.itemID, "status": "completed", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": it.text, "annotations": []any{}}}}
		s.output = append(s.output, item)
		var out []byte
		out = append(out, s.frame(map[string]any{"type": "response.output_text.done", "output_index": it.outIdx, "content_index": 0, "item_id": it.itemID, "text": it.text})...)
		out = append(out, s.frame(map[string]any{"type": "response.content_part.done", "output_index": it.outIdx, "content_index": 0, "item_id": it.itemID,
			"part": map[string]any{"type": "output_text", "text": it.text, "annotations": []any{}}})...)
		return append(out, s.frame(map[string]any{"type": "response.output_item.done", "output_index": it.outIdx, "item": item})...)
	case KindThinking:
		item := map[string]any{"type": "reasoning", "id": it.itemID, "summary": []map[string]any{{"type": "summary_text", "text": it.text}}}
		s.output = append(s.output, item)
		return s.frame(map[string]any{"type": "response.output_item.done", "output_index": it.outIdx, "item": item})
	case KindToolUse:
		// Drop the stale source→item mapping so a later ToolUseStop for this index is a no-op
		// instead of appending the tool call to the output array a second time.
		delete(s.srcToItem, it.srcIdx)
		return s.closeTool(it)
	}
	return nil
}

func (s *responsesStreamOut) closeTool(it *respOutItem) []byte {
	args := it.args
	if args == "" {
		args = "{}"
	}
	item := map[string]any{"type": "function_call", "id": it.itemID, "status": "completed", "call_id": it.callID, "name": it.name, "arguments": args}
	s.output = append(s.output, item)
	var out []byte
	out = append(out, s.frame(map[string]any{"type": "response.function_call_arguments.done", "output_index": it.outIdx, "item_id": it.itemID, "arguments": args})...)
	return append(out, s.frame(map[string]any{"type": "response.output_item.done", "output_index": it.outIdx, "item": item})...)
}
