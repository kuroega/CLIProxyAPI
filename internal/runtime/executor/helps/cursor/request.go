package cursor

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// ValidateTextRequest rejects content the native server-side agent cannot preserve.
func ValidateTextRequest(payload []byte, format string) error {
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return fmt.Errorf("request must be a JSON object")
	}
	root := gjson.ParseBytes(payload)
	for _, name := range []string{"previous_response_id", "conversation"} {
		if value := root.Get(name); value.Exists() && value.Type != gjson.Null && value.String() != "" {
			return fmt.Errorf("%s is unsupported; send the full text conversation", name)
		}
	}
	switch format {
	case "openai-response", "codex":
		if instruction := root.Get("instructions"); instruction.Exists() && instruction.Type != gjson.Null && instruction.Type != gjson.String {
			return fmt.Errorf("instructions must be text")
		}
		input := root.Get("input")
		if input.Type == gjson.String {
			if input.String() == "" {
				return fmt.Errorf("input must not be empty")
			}
			return nil
		}
		return validateMessages(input, false)
	case "openai", "claude":
		if system := root.Get("system"); system.Exists() {
			if err := validateTextContent(system); err != nil {
				return fmt.Errorf("system: %w", err)
			}
		}
		return validateMessages(root.Get("messages"), false)
	case "gemini":
		for _, name := range []string{"systemInstruction", "system_instruction"} {
			if system := root.Get(name); system.Exists() {
				if err := validateGeminiParts(system.Get("parts")); err != nil {
					return err
				}
			}
		}
		return validateMessages(root.Get("contents"), true)
	default:
		return fmt.Errorf("unsupported Cursor request format %q", format)
	}
}

func validateMessages(messages gjson.Result, gemini bool) error {
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return fmt.Errorf("a non-empty text conversation is required")
	}
	hasUser := false
	for _, message := range messages.Array() {
		if !message.IsObject() {
			return fmt.Errorf("conversation entries must be messages")
		}
		typ := message.Get("type").String()
		if typ != "" && typ != "message" {
			return fmt.Errorf("input item %q is unsupported", typ)
		}
		role := message.Get("role").String()
		if gemini && role == "model" {
			role = "assistant"
		}
		switch role {
		case "system", "developer", "user", "assistant":
		default:
			return fmt.Errorf("message role %q is unsupported", role)
		}
		if role == "user" {
			hasUser = true
		}
		for _, name := range []string{"tool_calls", "function_call", "tool_call_id", "refusal", "reasoning_content"} {
			value := message.Get(name)
			if value.Exists() && value.Type != gjson.Null && !(value.IsArray() && len(value.Array()) == 0) {
				return fmt.Errorf("message %s is unsupported", name)
			}
		}
		var err error
		if gemini {
			err = validateGeminiParts(message.Get("parts"))
		} else {
			err = validateTextContent(message.Get("content"))
		}
		if err != nil {
			return err
		}
	}
	if !hasUser {
		return fmt.Errorf("the conversation must contain a user message")
	}
	return nil
}

func validateTextContent(content gjson.Result) error {
	if content.Type == gjson.String {
		return nil
	}
	if !content.IsArray() || len(content.Array()) == 0 {
		return fmt.Errorf("message content must contain text")
	}
	for _, part := range content.Array() {
		typ := part.Get("type").String()
		if typ != "text" && typ != "input_text" && typ != "output_text" {
			return fmt.Errorf("content type %q is unsupported; Cursor accepts text only", typ)
		}
		if part.Get("text").Type != gjson.String {
			return fmt.Errorf("content text must be a string")
		}
	}
	return nil
}

func validateGeminiParts(parts gjson.Result) error {
	if !parts.IsArray() || len(parts.Array()) == 0 {
		return fmt.Errorf("Gemini parts must contain text")
	}
	for _, part := range parts.Array() {
		if !part.IsObject() || part.Get("text").Type != gjson.String {
			return fmt.Errorf("Cursor accepts only Gemini text parts")
		}
		invalid := false
		part.ForEach(func(key, value gjson.Result) bool {
			if key.String() != "text" {
				invalid = true
			}
			return true
		})
		if invalid {
			return fmt.Errorf("Cursor accepts only Gemini text parts")
		}
	}
	return nil
}
