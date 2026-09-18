package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---- Responses request → IR: real Codex traffic ---------------------------------------------

func TestResponsesToIR_Codex_ToolOutputFollowup(t *testing.T) {
	ir, err := openAIResponsesRequest{}.ToIR(fixture(t, "codex/02-tool-output-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ir.Model != "gpt-5" || !ir.Stream {
		t.Errorf("model/stream: %q %v", ir.Model, ir.Stream)
	}
	// Codex omits max_output_tokens → nil (DefaultMaxTokens must apply on the Anthropic side).
	if ir.MaxTokens != nil {
		t.Errorf("MaxTokens should be nil, got %v", *ir.MaxTokens)
	}
	// Codex sends tool_choice:"auto" explicitly; the IR normalises it to the zero value.
	if ir.ToolChoice.Mode != "" || ir.ParallelToolCalls == nil || !*ir.ParallelToolCalls {
		t.Errorf("tool_choice/parallel: %+v %v", ir.ToolChoice, ir.ParallelToolCalls)
	}
	// `instructions` → System; the developer item stays in Messages (they are distinct in Responses).
	if len(ir.System) != 1 || !strings.Contains(ir.System[0].Text, "You are a coding agent running in the Codex CLI") {
		t.Errorf("instructions → System: %d blocks", len(ir.System))
	}
	// namespace tools flattened; web_search dropped: 7 function + N nested.
	names := map[string]bool{}
	for _, tl := range ir.Tools {
		names[tl.Name] = true
	}
	if !names["exec_command"] || !names["close_agent"] {
		t.Errorf("tools not flattened (want exec_command and nested close_agent): %v", names)
	}
	for _, tl := range ir.Tools {
		if tl.Name == "" || !json.Valid(tl.Schema) {
			t.Errorf("bad tool def: %+v", tl)
		}
	}
	// Messages: system(developer, 2 blocks) / user(env) / user(prompt) / assistant(text + tool_use merged) / tool(result)
	wantRoles := []Role{RoleSystem, RoleUser, RoleUser, RoleAssistant, RoleTool}
	if len(ir.Messages) != len(wantRoles) {
		t.Fatalf("roles: %v", roles(ir.Messages))
	}
	for i, r := range wantRoles {
		if ir.Messages[i].Role != r {
			t.Errorf("messages[%d] = %q want %q", i, ir.Messages[i].Role, r)
		}
	}
	if dev := ir.Messages[0].Content; len(dev) != 2 || !strings.Contains(dev[0].Text, "<skills_instructions>") {
		t.Errorf("developer item must keep its 2 input_text blocks: %d", len(dev))
	}
	as := ir.Messages[3].Content
	if len(as) != 2 || as[0].Kind != KindText || as[1].Kind != KindToolUse {
		t.Fatalf("assistant must merge output_text + function_call: %v", kinds(as))
	}
	if as[1].ToolCallID != "call_af2663c77e305207babb5ac361f950ea" || as[1].ToolName != "exec_command" {
		t.Errorf("function_call → tool_use uses call_id (not id): %+v", as[1])
	}
	var args map[string]any
	if json.Unmarshal(as[1].ToolInput, &args) != nil || args["cmd"] != "wc -l notes.txt" {
		t.Errorf("arguments string → JSON object: %s", as[1].ToolInput)
	}
	tr := ir.Messages[4].Content[0]
	if tr.ToolResultID != as[1].ToolCallID {
		t.Errorf("function_call_output.call_id must match: %q vs %q", tr.ToolResultID, as[1].ToolCallID)
	}
	var s string
	if json.Unmarshal(tr.ToolResult, &s) != nil || !strings.Contains(s, "4 notes.txt") {
		t.Errorf("tool output: %s", tr.ToolResult)
	}
}

func TestResponsesToIR_RejectsPreviousResponseID(t *testing.T) {
	_, err := openAIResponsesRequest{}.ToIR([]byte(`{"model":"m","previous_response_id":"resp_1","input":"hi"}`))
	if !errors.Is(err, ErrStatefulResponses) {
		t.Fatalf("stateful request must be rejected clearly, got %v", err)
	}
}

func TestResponsesToIR_StringInput(t *testing.T) {
	ir, err := openAIResponsesRequest{}.ToIR([]byte(`{"model":"m","input":"hello","instructions":"be brief","max_output_tokens":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) != 1 || ir.Messages[0].Role != RoleUser || ir.Messages[0].Content[0].Text != "hello" {
		t.Errorf("string input: %+v", ir.Messages)
	}
	if len(ir.System) != 1 || ir.System[0].Text != "be brief" || *ir.MaxTokens != 50 {
		t.Errorf("instructions/max: %+v %v", ir.System, ir.MaxTokens)
	}
}

// ---- Scenario 2 request path: Codex (Responses) → IR → Anthropic ------------------------------

func TestResponsesToAnthropic_Codex(t *testing.T) {
	ir, err := openAIResponsesRequest{}.ToIR(fixture(t, "codex/02-tool-output-followup.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := anthropicRequest{}.FromIR(ir, "us.anthropic.claude-sonnet-5", Options{DefaultMaxTokens: DefaultMaxTokens})
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["model"] != "us.anthropic.claude-sonnet-5" || m["stream"] != true {
		t.Errorf("top-level: %v %v", m["model"], m["stream"])
	}
	// Codex sent no max_output_tokens → default applied (Anthropic requires it).
	if m["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("default max_tokens: %v", m["max_tokens"])
	}
	// instructions (1 block) + leading developer item (2 blocks) → Anthropic top-level system (3 blocks).
	sys := m["system"].([]any)
	if len(sys) != 3 || !strings.Contains(sys[0].(map[string]any)["text"].(string), "You are a coding agent") || !strings.Contains(sys[1].(map[string]any)["text"].(string), "<skills_instructions>") {
		t.Errorf("instructions + leading developer must become Anthropic system (3 blocks): %d", len(sys))
	}
	msgs := m["messages"].([]any)
	// user(env)+user(prompt) merge (Anthropic forbids consecutive same role) / assistant / user(tool_result)
	wantRoles := []string{"user", "assistant", "user"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("want %v got %v", wantRoles, chatRoles(msgs))
	}
	u0 := msgs[0].(map[string]any)["content"].([]any)
	if len(u0) != 2 {
		t.Errorf("two consecutive Codex user messages must merge into one Anthropic user turn with 2 blocks, got %d", len(u0))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 2 || asst[0].(map[string]any)["type"] != "text" || asst[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant blocks: %v", asst)
	}
	tu := asst[1].(map[string]any)
	if tu["id"] != "call_af2663c77e305207babb5ac361f950ea" || tu["name"] != "exec_command" {
		t.Errorf("tool_use: %v", tu)
	}
	if inp, _ := tu["input"].(map[string]any); inp == nil || inp["cmd"] != "wc -l notes.txt" {
		t.Errorf("tool_use.input must be an object: %v", tu["input"])
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != tu["id"] || !strings.Contains(tr["content"].(string), "4 notes.txt") {
		t.Errorf("tool_result: %v", tr)
	}
	tools := m["tools"].([]any)
	if len(tools) < 8 { // 7 top-level functions + flattened namespace members
		t.Errorf("want ≥8 tools after flattening namespace, got %d", len(tools))
	}
	t0 := tools[0].(map[string]any)
	if t0["input_schema"] == nil || t0["name"] == nil {
		t.Errorf("anthropic tool def: %v", t0)
	}
	// Responses-only fields must not leak.
	for _, k := range []string{"input", "instructions", "store", "include", "reasoning", "previous_response_id", "parallel_tool_calls", "max_output_tokens", "client_metadata", "prompt_cache_key"} {
		if _, has := m[k]; has {
			t.Errorf("responses field %q leaked into anthropic request", k)
		}
	}
	if m["tool_choice"] != nil {
		t.Errorf("tool_choice auto should be omitted, got %v", m["tool_choice"])
	}
}

// ---- Responses round trip: Codex request → IR → Responses → IR must be structurally equal ------

func TestResponsesRoundTrip_Codex(t *testing.T) {
	for _, f := range []string{"01-tool-call", "02-tool-output-followup"} {
		t.Run(f, func(t *testing.T) {
			in := fixture(t, "codex/"+f+".request.json")
			ir, err := openAIResponsesRequest{}.ToIR(in)
			if err != nil {
				t.Fatal(err)
			}
			out, err := openAIResponsesRequest{}.FromIR(ir, ir.Model, Options{})
			if err != nil {
				t.Fatal(err)
			}
			ir2, err := openAIResponsesRequest{}.ToIR(out)
			if err != nil {
				t.Fatalf("re-parse: %v\n%.300s", err, out)
			}
			a, _ := json.Marshal(ir)
			b, _ := json.Marshal(ir2)
			if string(a) != string(b) {
				t.Errorf("IR changed across round trip\n first: %.300s\nsecond: %.300s", a, b)
			}
			m := mustJSON(t, out)
			if m["store"] != false {
				t.Errorf("FromIR must emit store:false (stateless): %v", m["store"])
			}
		})
	}
}

// ---- Responses non-stream response → IR → Anthropic response ---------------------------------

func TestResponsesResponseToAnthropic(t *testing.T) {
	body := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-x",
	  "output":[
	    {"type":"reasoning","id":"rs_1","encrypted_content":"ENC==","summary":[{"type":"summary_text","text":"thinking about it"}]},
	    {"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Running it.","annotations":[]}]},
	    {"type":"function_call","id":"fc_1","status":"completed","call_id":"call_9","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}
	  ],
	  "usage":{"input_tokens":120,"output_tokens":40,"input_tokens_details":{"cached_tokens":100},"output_tokens_details":{"reasoning_tokens":25},"total_tokens":160}}`
	ir, err := openAIResponsesResponse{}.ToIR([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if ir.StopReason != StopToolUse {
		t.Errorf("completed + function_call → StopToolUse, got %q", ir.StopReason)
	}
	if k := kinds(ir.Content); len(k) != 3 || k[0] != KindThinking || k[1] != KindText || k[2] != KindToolUse {
		t.Fatalf("kinds: %v", k)
	}
	if !strings.Contains(string(ir.Content[0].ThinkingRaw), "ENC==") {
		t.Error("encrypted_content must be preserved opaquely in ThinkingRaw")
	}
	// Responses input_tokens=120 (includes cached 100) → IR Input=20, CacheRead=100.
	if ir.Usage.Input != 20 || ir.Usage.CacheRead != 100 || ir.Usage.Output != 40 || ir.Usage.Reasoning != 25 {
		t.Errorf("usage: %+v", ir.Usage)
	}
	out, err := anthropicResponse{}.FromIR(ir)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	content := m["content"].([]any)
	// The OpenAI reasoning item has no Anthropic signature → not emitted as a thinking block.
	if len(content) != 2 || content[0].(map[string]any)["type"] != "text" || content[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("anthropic content: %v", content)
	}
	if bytes.Contains(out, []byte("ENC==")) {
		t.Error("OpenAI encrypted reasoning leaked into Anthropic response")
	}
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != float64(20) || u["cache_read_input_tokens"] != float64(100) {
		t.Errorf("anthropic usage: %v", u)
	}
}

// ---- Responses SSE → IR: real Codex/Bedrock stream ----------------------------------------------

func TestResponsesStreamToIR_RealCodexToolCall(t *testing.T) {
	frames := parseSSE(t, fixture(t, "codex/01-tool-call.response.sse"))
	evs := driveIn(t, newResponsesStreamIn(), frames)
	ks := kindSeq(evs)
	if len(ks) == 0 || ks[0] != EventMessageStart {
		t.Fatalf("must start with message_start: %v", ks)
	}
	var text strings.Builder
	var start *StreamEvent
	stops := 0
	for i := range evs {
		e := evs[i]
		switch e.Kind {
		case EventTextDelta:
			text.WriteString(e.Text)
		case EventToolUseStart:
			start = &evs[i]
		case EventMessageStop:
			stops++
		}
	}
	if text.String() != "I’ll count the lines in `notes.txt` now." {
		t.Errorf("text: %q", text.String())
	}
	if start == nil || start.ToolCallID != "call_af2663c77e305207babb5ac361f950ea" || start.ToolName != "exec_command" || start.Index != 1 {
		t.Fatalf("tool_use_start: %+v", start)
	}
	args := joinToolInput(evs, 1)
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m["cmd"] != "wc -l notes.txt" {
		t.Errorf("38 argument fragments must reassemble: %q", args)
	}
	// The recording was cut by the client right after response.completed's first bytes, so the
	// terminal frame is truncated in this fixture; the stream must still not produce garbage.
	if stops > 1 {
		t.Errorf("at most one message_stop, got %d", stops)
	}
}

// ---- Scenario 2 return path: real Anthropic stream → IR → Responses SSE for Codex ----------------

func TestAnthropicStreamToResponsesSSE(t *testing.T) {
	frames := parseSSE(t, fixture(t, "claude-code/02-tool-use-call.response.sse"))
	evs := driveIn(t, newAnthropicStreamIn(), frames)
	out := driveOut(t, newResponsesStreamOut(), evs)

	got := parseSSE(t, out)
	var types []string
	seqOK := true
	var seqs []int
	for _, f := range got {
		var d struct {
			Type        string         `json:"type"`
			Seq         *int           `json:"sequence_number"`
			OutputIndex *int           `json:"output_index"`
			Item        map[string]any `json:"item"`
			Response    map[string]any `json:"response"`
			Delta       string         `json:"delta"`
			ItemID      string         `json:"item_id"`
		}
		if err := json.Unmarshal([]byte(f.Data), &d); err != nil {
			t.Fatalf("bad frame: %v\n%s", err, f.Data)
		}
		types = append(types, d.Type)
		if d.Seq == nil {
			seqOK = false
		} else {
			seqs = append(seqs, *d.Seq)
		}
	}
	if !seqOK {
		t.Error("every frame needs sequence_number")
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Errorf("sequence_number not contiguous at %d: %v", i, seqs)
			break
		}
	}
	if types[0] != "response.created" || types[1] != "response.in_progress" || types[len(types)-1] != "response.completed" {
		t.Fatalf("envelope events: first=%v last=%v", types[:2], types[len(types)-1])
	}
	// Anthropic thinking (no Responses equivalent with signature) must not become a bogus
	// encrypted reasoning item; a summary-only reasoning item or nothing is acceptable. The
	// tool_use must become a complete function_call item family.
	var addedTypes []string
	var argsDone, itemsDone int
	var argFrag strings.Builder
	var finalOutput []any
	var usage map[string]any
	for _, f := range got {
		var d struct {
			Type     string         `json:"type"`
			Item     map[string]any `json:"item"`
			Delta    string         `json:"delta"`
			Response map[string]any `json:"response"`
		}
		_ = json.Unmarshal([]byte(f.Data), &d)
		switch d.Type {
		case "response.output_item.added":
			addedTypes = append(addedTypes, d.Item["type"].(string))
			if d.Item["type"] == "function_call" {
				if d.Item["call_id"] != "toolu_bdrk_01DvBxF543NYes3fUgqodtEL" || d.Item["name"] != "Read" || d.Item["status"] != "in_progress" {
					t.Errorf("function_call added: %v", d.Item)
				}
			}
		case "response.function_call_arguments.delta":
			argFrag.WriteString(d.Delta)
		case "response.function_call_arguments.done":
			argsDone++
		case "response.output_item.done":
			itemsDone++
		case "response.completed":
			finalOutput, _ = d.Response["output"].([]any)
			usage, _ = d.Response["usage"].(map[string]any)
			if d.Response["status"] != "completed" {
				t.Errorf("status: %v", d.Response["status"])
			}
		}
	}
	if len(addedTypes) == 0 || addedTypes[len(addedTypes)-1] != "function_call" {
		t.Errorf("output items added: %v", addedTypes)
	}
	if argsDone != 1 || itemsDone < 1 {
		t.Errorf("function_call_arguments.done=%d output_item.done=%d", argsDone, itemsDone)
	}
	var m map[string]any
	if json.Unmarshal([]byte(argFrag.String()), &m) != nil || m["file_path"] == nil {
		t.Errorf("arguments delta reassembled: %q", argFrag.String())
	}
	// response.completed must carry the full output[] (Codex reads it) and usage.
	foundFC := false
	for _, it := range finalOutput {
		im := it.(map[string]any)
		if im["type"] == "function_call" && im["status"] == "completed" && json.Valid([]byte(im["arguments"].(string))) {
			foundFC = true
		}
	}
	if !foundFC {
		t.Errorf("response.completed.output must contain the completed function_call: %v", finalOutput)
	}
	// Anthropic input=2 + cache_write=38813 → Responses input_tokens=38815.
	if usage == nil || usage["input_tokens"] != float64(38815) || usage["output_tokens"] != float64(88) {
		t.Errorf("usage: %v", usage)
	}
	if bytes.Contains(out, []byte("EqUCCpEBCBEQ")) || bytes.Contains(out, []byte("message_start")) {
		t.Error("Anthropic signature or framing leaked into Responses stream")
	}
}

// ---- Responses stream round trip through IR -----------------------------------------------------

func TestResponsesStreamRoundTrip(t *testing.T) {
	frames := parseSSE(t, fixture(t, "codex/01-tool-call.response.sse"))
	evs := driveIn(t, newResponsesStreamIn(), frames)
	// Some fixtures end mid-frame (client abort); ensure a terminal event exists for the encoder.
	if evs[len(evs)-1].Kind != EventMessageStop {
		evs = append(evs, StreamEvent{Kind: EventMessageStop, StopReason: StopToolUse})
	}
	out := driveOut(t, newResponsesStreamOut(), evs)
	evs2 := driveIn(t, newResponsesStreamIn(), parseSSE(t, out))
	if strings.Join(toStrings(kindSeq(filterKinds(evs))), ",") != strings.Join(toStrings(kindSeq(filterKinds(evs2))), ",") {
		t.Errorf("IR kinds differ after round trip\n1: %v\n2: %v", kindSeq(evs), kindSeq(evs2))
	}
	if joinToolInput(evs, 1) != joinToolInput(evs2, 1) {
		t.Errorf("tool input differs: %q vs %q", joinToolInput(evs, 1), joinToolInput(evs2, 1))
	}
}

// filterKinds drops events that legitimately vary in count across a round trip (text delta
// granularity is a rendering choice, not semantics).
func filterKinds(evs []StreamEvent) []StreamEvent {
	var out []StreamEvent
	for _, e := range evs {
		if e.Kind == EventTextDelta || e.Kind == EventToolInputDelta {
			continue
		}
		out = append(out, e)
	}
	return out
}
