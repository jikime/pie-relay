package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// naiveUnmarshalMessageType reproduces the ORIGINAL peekMessageType
// implementation (full json.Unmarshal into a struct) purely so the benchmark
// below can demonstrate, empirically, how much the streaming-decoder version
// saves on large non-matching payloads. It is not used anywhere outside this
// benchmark.
func naiveUnmarshalMessageType(data []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.Type
}

// largeToolResultPayload simulates a realistic large message this relay must
// carry (e.g. a big tool_result dump) — "type" is present and first (as in
// every message shape this protocol produces today) but is NOT one of the 3
// logged values, and the bulk of the payload is an unrelated large field.
func largeToolResultPayload(sizeBytes int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"tool_result","output":"`)
	b.WriteString(strings.Repeat("x", sizeBytes))
	b.WriteString(`"}`)
	return []byte(b.String())
}

func BenchmarkPeekMessageType_Streaming_1MB(b *testing.B) {
	data := largeToolResultPayload(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		peekMessageType(data)
	}
}

func BenchmarkPeekMessageType_NaiveUnmarshal_1MB(b *testing.B) {
	data := largeToolResultPayload(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		naiveUnmarshalMessageType(data)
	}
}

func BenchmarkPeekMessageType_Streaming_8MB(b *testing.B) {
	data := largeToolResultPayload(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		peekMessageType(data)
	}
}

func BenchmarkPeekMessageType_NaiveUnmarshal_8MB(b *testing.B) {
	data := largeToolResultPayload(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		naiveUnmarshalMessageType(data)
	}
}

func BenchmarkPeekMessageType_Streaming_Small(b *testing.B) {
	data := []byte(`{"type":"chat","prompt":"hello"}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		peekMessageType(data)
	}
}
