package capture

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type ResponseMode string

const (
	ResponseText       ResponseMode = "text"
	ResponseFunction   ResponseMode = "function"
	ResponseParallel   ResponseMode = "parallel"
	ResponseCustom     ResponseMode = "custom"
	ResponseIncomplete ResponseMode = "incomplete"
	ResponseFailed     ResponseMode = "failed"
)

func ParseResponseMode(value string) (ResponseMode, error) {
	mode := ResponseMode(value)
	switch mode {
	case ResponseText, ResponseFunction, ResponseParallel, ResponseCustom, ResponseIncomplete, ResponseFailed:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported response mode %q", value)
	}
}

func eventTypes(events []map[string]any) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		if eventType, ok := event["type"].(string); ok {
			result = append(result, eventType)
		}
	}
	return result
}

func responseEvents(mode ResponseMode, model, text string, sequence int) ([]map[string]any, error) {
	responseID := fmt.Sprintf("resp_capture_%04d", sequence)
	messageID := fmt.Sprintf("msg_capture_%04d", sequence)
	callID := fmt.Sprintf("call_capture_%04d", sequence)
	base := responseObject(responseID, model, "in_progress", nil, nil)
	events := []map[string]any{
		{"type": "response.created", "response": base, "sequence_number": 0},
		{"type": "response.in_progress", "response": base, "sequence_number": 1},
	}

	switch mode {
	case ResponseText:
		return append(events, textEvents(responseID, messageID, model, text, "completed", sequence)...), nil
	case ResponseFunction:
		return append(events, functionEvents(responseID, callID, model, 0, "exec_command", `{"cmd":"printf capture-tool-output"}`, sequence)...), nil
	case ResponseParallel:
		return append(events, parallelFunctionEvents(responseID, model, sequence)...), nil
	case ResponseCustom:
		return append(events, customEvents(responseID, callID, model, sequence)...), nil
	case ResponseIncomplete:
		base = responseObject(responseID, model, "incomplete", nil, map[string]any{"reason": "max_output_tokens"})
		return append(events, map[string]any{"type": "response.incomplete", "response": base, "sequence_number": 2}), nil
	case ResponseFailed:
		base = responseObject(responseID, model, "failed", map[string]any{"code": "server_error", "message": "capture server failure fixture"}, nil)
		return append(events, map[string]any{"type": "response.failed", "response": base, "sequence_number": 2}), nil
	default:
		return nil, fmt.Errorf("unsupported response mode %q", mode)
	}
}

func textEvents(responseID, messageID, model, text, status string, _ int) []map[string]any {
	if text == "" {
		text = "capture acknowledged"
	}
	itemAdded := map[string]any{
		"id": messageID, "status": "in_progress", "type": "message", "role": "assistant", "content": []any{},
	}
	itemDone := map[string]any{
		"id": messageID, "status": "completed", "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	response := responseObject(responseID, model, status, nil, nil)
	response["output"] = []any{itemDone}
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": itemAdded, "sequence_number": 2},
		{"type": "response.content_part.added", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}, "sequence_number": 3},
		{"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0, "delta": text, "logprobs": []any{}, "sequence_number": 4},
		{"type": "response.output_text.done", "item_id": messageID, "output_index": 0, "content_index": 0, "text": text, "sequence_number": 5},
		{"type": "response.content_part.done", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}, "sequence_number": 6},
		{"type": "response.output_item.done", "output_index": 0, "item": itemDone, "sequence_number": 7},
		{"type": "response." + status, "response": response, "sequence_number": 8},
	}
}

func functionEvents(responseID, callID, model string, outputIndex int, name, arguments string, _ int) []map[string]any {
	itemID := fmt.Sprintf("fc_capture_%02d", outputIndex)
	itemAdded := map[string]any{
		"id": itemID, "type": "function_call", "status": "in_progress", "call_id": callID,
		"name": name, "arguments": "",
	}
	itemDone := map[string]any{
		"id": itemID, "type": "function_call", "status": "completed", "call_id": callID,
		"name": name, "arguments": arguments,
	}
	response := responseObject(responseID, model, "completed", nil, nil)
	response["output"] = []any{itemDone}
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": outputIndex, "item": itemAdded, "sequence_number": 2},
		{"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": outputIndex, "delta": arguments, "sequence_number": 3},
		{"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": outputIndex, "name": name, "arguments": arguments, "sequence_number": 4},
		{"type": "response.output_item.done", "output_index": outputIndex, "item": itemDone, "sequence_number": 5},
		{"type": "response.completed", "response": response, "sequence_number": 6},
	}
}

func parallelFunctionEvents(responseID, model string, _ int) []map[string]any {
	first := functionEvents(responseID, "call_capture_0001", model, 0, "exec_command", `{"cmd":"true"}`, 0)
	second := functionEvents(responseID, "call_capture_0002", model, 1, "exec_command", `{"cmd":"printf parallel-tool-output"}`, 0)
	result := make([]map[string]any, 0, len(first)+len(second)-1)
	for _, event := range []map[string]any{
		first[0], second[0],
		first[1], second[1],
		first[2], second[2],
		first[3], second[3],
	} {
		copyEvent := cloneEvent(event)
		copyEvent["sequence_number"] = len(result) + 2
		result = append(result, copyEvent)
	}
	response := responseObject(responseID, model, "completed", nil, nil)
	response["output"] = []any{first[3]["item"], second[3]["item"]}
	result = append(result, map[string]any{"type": "response.completed", "response": response, "sequence_number": len(result) + 2})
	return result
}

func customEvents(responseID, callID, model string, _ int) []map[string]any {
	itemID := "ctc_capture_00"
	input := "*** Begin Patch\n*** End Patch"
	itemAdded := map[string]any{
		"id": itemID, "type": "custom_tool_call", "status": "in_progress", "call_id": callID,
		"name": "apply_patch", "input": "",
	}
	itemDone := map[string]any{
		"id": itemID, "type": "custom_tool_call", "status": "completed", "call_id": callID,
		"name": "apply_patch", "input": input,
	}
	response := responseObject(responseID, model, "completed", nil, nil)
	response["output"] = []any{itemDone}
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": itemAdded, "sequence_number": 2},
		{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": itemID, "delta": input, "sequence_number": 3},
		{"type": "response.custom_tool_call_input.done", "output_index": 0, "item_id": itemID, "input": input, "sequence_number": 4},
		{"type": "response.output_item.done", "output_index": 0, "item": itemDone, "sequence_number": 5},
		{"type": "response.completed", "response": response, "sequence_number": 6},
	}
}

func responseObject(id, model, status string, responseError, incompleteDetails map[string]any) map[string]any {
	result := map[string]any{
		"id": id, "object": "response", "created_at": 0, "status": status, "completed_at": nil,
		"error": responseError, "incomplete_details": incompleteDetails, "instructions": nil,
		"max_output_tokens": nil, "model": model, "output": []any{}, "parallel_tool_calls": true,
		"previous_response_id": nil, "reasoning": map[string]any{"effort": nil, "summary": nil},
		"store": false, "text": map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice": "auto", "tools": []any{}, "top_p": 1, "truncation": "disabled", "usage": nil,
		"user": nil, "metadata": map[string]any{},
	}
	if status == "completed" {
		result["completed_at"] = 0
	}
	return result
}

func cloneEvent(event map[string]any) map[string]any {
	clone := make(map[string]any, len(event))
	for key, value := range event {
		clone[key] = value
	}
	return clone
}

func writeResponseStream(w http.ResponseWriter, events []map[string]any) ([]string, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support streaming")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writtenTypes := make([]string, 0, len(events))
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			return writtenTypes, err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return writtenTypes, err
		}
		if eventType, ok := event["type"].(string); ok {
			writtenTypes = append(writtenTypes, eventType)
		}
		flusher.Flush()
	}
	return writtenTypes, nil
}
