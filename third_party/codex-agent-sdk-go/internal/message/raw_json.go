package message

import "encoding/json"

const rawJSONKey = "\x00sdk_raw_json"

// AnnotateRawJSON attaches the original raw JSON bytes to a decoded payload so
// audit envelopes can preserve byte fidelity later in the parse pipeline.
func AnnotateRawJSON(data map[string]any, raw []byte) map[string]any {
	if data == nil || len(raw) == 0 {
		return data
	}

	data[rawJSONKey] = append([]byte(nil), raw...)

	return data
}

func extractRawJSON(data map[string]any) (json.RawMessage, bool) {
	if data == nil {
		return nil, false
	}

	switch raw := data[rawJSONKey].(type) {
	case []byte:
		if len(raw) == 0 {
			return nil, false
		}

		return append(json.RawMessage(nil), raw...), true
	case json.RawMessage:
		if len(raw) == 0 {
			return nil, false
		}

		return append(json.RawMessage(nil), raw...), true
	case string:
		if raw == "" {
			return nil, false
		}

		return json.RawMessage(raw), true
	default:
		return nil, false
	}
}

func stripRawJSON(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}

	if _, ok := data[rawJSONKey]; !ok {
		return data
	}

	sanitized := make(map[string]any, len(data)-1)
	for key, value := range data {
		if key == rawJSONKey {
			continue
		}

		sanitized[key] = value
	}

	return sanitized
}
