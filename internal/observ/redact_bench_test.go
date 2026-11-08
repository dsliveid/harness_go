package observ

import (
	"strings"
	"testing"
)

// 脱敏跑在**每一次** LLM 调用的写入热路径上（8.3 万次/天）。
// 若单次脱敏耗时进入毫秒级，写入管道就会成为吞吐瓶颈（实测曾把 preview 档
// 拖慢到 154 行/秒，而 off 档有 8500 行/秒）。本基准用于守住这条性能红线。

// benchPayload 构造接近真实的报文（12K token 量级，含中文）。
func benchPayload() string {
	body := make([]byte, 0, 42000)
	body = append(body, `{"model":"gpt-4","messages":[{"role":"user","content":"`...)
	for len(body) < 40000 {
		body = append(body, "这是一段测试用的上下文内容，用于模拟真实的长对话请求体。"...)
	}
	body = append(body, `"}]}`...)
	return string(body)
}

// BenchmarkRedactPayload 无凭据的长报文脱敏（最常见路径）。
//
// 目标：< 200µs（12K token 报文）。超过这个量级就会成为写入瓶颈。
func BenchmarkRedactPayload(b *testing.B) {
	payload := benchPayload()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := RedactPayload(payload)
		if len(out) == 0 {
			b.Fatal("脱敏结果不应为空")
		}
	}
}

// BenchmarkRedactPayloadWithSecret 含凭据的长报文（最坏路径：所有规则都命中）。
func BenchmarkRedactPayloadWithSecret(b *testing.B) {
	payload := benchPayload() + ` password="hunter2" api_key="abc123secret"` +
		` Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9` +
		` sk-abcdefghijklmnop1234567890`
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactPayload(payload)
	}
}

// BenchmarkRedactPayloadShort 短报文（多数工具调用参数的量级）。
func BenchmarkRedactPayloadShort(b *testing.B) {
	payload := strings.Repeat("short payload ", 50) // ≈700B
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactPayload(payload)
	}
}
