package log

import (
	"encoding/json"
	"time"
)

// jsonRecord is the shape of every JSON line emitted by a Logger in
// FormatJSON mode. Keep field names stable across versions — they are
// the public contract for log shippers (Loki labels, ES mappings).
type jsonRecord struct {
	TS     string         `json:"ts"`
	Level  string         `json:"level"`
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields,omitempty"`
}

// writeJSON renders a single record. Marshalling and the field copy
// happen outside the mutex so concurrent log lines only contend on
// the single Write that follows. A marshal failure is silently
// dropped — the pkg/log convention is "log lines never crash the
// producer".
func (l *Logger) writeJSON(level Level, msg string) {
	rec := jsonRecord{
		TS:    time.Now().UTC().Format(time.RFC3339Nano),
		Level: level.String(),
		Msg:   msg,
	}
	if len(l.fields) > 0 {
		rec.Fields = make(map[string]any, len(l.fields))
		for k, v := range l.fields {
			rec.Fields[k] = v
		}
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return
	}
	body = append(body, '\n')

	l.mu.Lock()
	_, _ = l.w.Write(body)
	l.mu.Unlock()
}
