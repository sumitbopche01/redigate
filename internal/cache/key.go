// Package cache identifies cacheable chat completions and stores their
// upstream responses in Redis.
//
// key.go owns exact-match identity. Normalization is applied only when
// building the key; callers forward the original request body upstream.
package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeyPrefix is prepended to every chat cache key. Bump the version segment
// when normalization changes so old entries are ignored.
const KeyPrefix = "redigate:chat:v1:"

// Identity is the cache decision for one request body.
type Identity struct {
	// Cacheable is true for non-streaming calls whose temperature is omitted
	// or zero. The key is still populated when Cacheable is false so tests can
	// show temperature and stream are not part of the hash. Callers must not
	// read or write the cache unless Cacheable is true.
	Cacheable bool
	Key       string
	Model     string
}

// ValidationError is a caller mistake (bad JSON or a missing field).
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

func invalid(msg string) error { return &ValidationError{Msg: msg} }

// Inspect parses an OpenAI chat-completion body and returns its cache identity.
//
// A request is cacheable when stream is absent or false and temperature is
// absent or 0. The key hashes a canonical document of:
//   - model, trimmed
//   - messages, with role trimmed and lowercased, string content and text
//     parts trimmed, and empty names removed
//   - every other top-level field except temperature and stream
func Inspect(body []byte) (Identity, error) {
	cacheable, blob, model, err := material(body)
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256(blob)
	return Identity{
		Cacheable: cacheable,
		Key:       KeyPrefix + hex.EncodeToString(sum[:]),
		Model:     model,
	}, nil
}

func material(body []byte) (cacheable bool, blob []byte, model string, err error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return false, nil, "", invalid("invalid JSON body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return false, nil, "", invalid("invalid JSON body")
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return false, nil, "", invalid("body must be a JSON object")
	}

	model, err = parseModel(obj["model"], has(obj, "model"))
	if err != nil {
		return false, nil, "", err
	}
	messages, err := parseMessages(obj["messages"], has(obj, "messages"))
	if err != nil {
		return false, nil, "", err
	}
	temp, err := parseTemperature(obj["temperature"], has(obj, "temperature"))
	if err != nil {
		return false, nil, "", err
	}
	stream, err := parseStream(obj["stream"], has(obj, "stream"))
	if err != nil {
		return false, nil, "", err
	}

	extras := make(map[string]any)
	for k, v := range obj {
		switch k {
		case "model", "messages", "temperature", "stream":
			continue
		default:
			extras[k] = v
		}
	}

	doc := map[string]any{
		"model":    model,
		"messages": messages,
	}
	if len(extras) > 0 {
		doc["extras"] = extras
	}
	blob, err = marshalCanonical(doc)
	if err != nil {
		return false, nil, "", fmt.Errorf("canonical cache key: %w", err)
	}
	return !stream && temperatureIsZero(temp), blob, model, nil
}

func has(obj map[string]any, key string) bool {
	_, ok := obj[key]
	return ok
}

func parseModel(v any, present bool) (string, error) {
	if !present || v == nil {
		return "", invalid("model is required")
	}
	s, ok := v.(string)
	if !ok {
		return "", invalid("model must be a string")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", invalid("model is required")
	}
	return s, nil
}

func parseMessages(v any, present bool) ([]any, error) {
	if !present || v == nil {
		return nil, invalid("messages is required")
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, invalid("messages must be an array")
	}
	if len(arr) == 0 {
		return nil, invalid("messages is required")
	}
	out := make([]any, len(arr))
	for i, item := range arr {
		msg, err := normalizeMessage(item)
		if err != nil {
			return nil, err
		}
		out[i] = msg
	}
	return out, nil
}

func normalizeMessage(v any) (map[string]any, error) {
	src, ok := v.(map[string]any)
	if !ok {
		return nil, invalid("each message must be an object")
	}
	dst := make(map[string]any, len(src))
	for k, val := range src {
		dst[k] = val
	}
	role, ok := dst["role"].(string)
	if !ok {
		return nil, invalid("message role must be a string")
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return nil, invalid("message role is required")
	}
	dst["role"] = role
	if c, exists := dst["content"]; exists && c != nil {
		dst["content"] = normalizeContent(c)
	}
	if name, ok := dst["name"].(string); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			delete(dst, "name")
		} else {
			dst["name"] = name
		}
	}
	return dst, nil
}

func normalizeContent(c any) any {
	switch t := c.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		out := make([]any, len(t))
		for i, part := range t {
			pm, ok := part.(map[string]any)
			if !ok {
				out[i] = part
				continue
			}
			cp := make(map[string]any, len(pm))
			for k, v := range pm {
				cp[k] = v
			}
			if text, ok := cp["text"].(string); ok {
				cp["text"] = strings.TrimSpace(text)
			}
			out[i] = cp
		}
		return out
	default:
		return c
	}
}

func parseTemperature(v any, present bool) (*float64, error) {
	if !present || v == nil {
		return nil, nil
	}
	num, ok := v.(json.Number)
	if !ok {
		return nil, invalid("temperature must be a number")
	}
	f, err := num.Float64()
	if err != nil || f < 0 {
		return nil, invalid("temperature must be a non-negative number")
	}
	return &f, nil
}

func temperatureIsZero(v *float64) bool {
	return v == nil || *v == 0
}

func parseStream(v any, present bool) (bool, error) {
	if !present || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, invalid("stream must be a boolean")
	}
	return b, nil
}

func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// IsValidation reports whether err is a request-validation failure.
func IsValidation(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}
