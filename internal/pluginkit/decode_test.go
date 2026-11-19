package pluginkit

import (
	"errors"
	"strings"
	"testing"

	"harness-go/pkg/toolapi"
)

type strictArgs struct {
	OffsetLine int    `json:"offset_line"`
	MaxLines   int    `json:"max_lines"`
	Path       string `json:"path"`
}

// TestDecodeArgsRejectsUnknownField 未知字段必须报错（不再静默丢弃）。
//
// 这是本包最重要的不变量：宽松 json.Unmarshal 会把参数名写错的调用
// 变成"不报错但参数全失效"，那类故障排查成本极高（看起来像功能坏了）。
func TestDecodeArgsRejectsUnknownField(t *testing.T) {
	var a strictArgs
	err := DecodeArgs([]byte(`{"path":"x","offset":8,"limit":2}`), &a)
	if err == nil {
		t.Fatal("未知字段 offset/limit 必须报错，而非静默丢弃")
	}
	if !errors.Is(err, toolapi.ErrInvalidArgs) {
		t.Fatalf("应包装为 ErrInvalidArgs，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "未知参数") {
		t.Fatalf("错误信息应说明是未知参数: %v", err)
	}
}

// TestDecodeArgsErrorListsAvailableFields 错误必须给出可用字段名。
//
// 只说"未知字段"模型还得猜；给出可用清单后下一轮就能改对，
// 省下的是一整轮真实 token。
func TestDecodeArgsErrorListsAvailableFields(t *testing.T) {
	var a strictArgs
	err := DecodeArgs([]byte(`{"offset":8}`), &a)
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	for _, want := range []string{"max_lines", "offset_line", "path"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应列出可用字段 %q: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "lookup_tool") {
		t.Fatalf("应指引用 lookup_tool 查规范: %s", msg)
	}
}

// TestDecodeArgsAcceptsValidAndEmpty 合法参数与空参数都必须通过。
//
// 空参数是合法的（全默认值工具），不能因为严格模式被误伤。
func TestDecodeArgsAcceptsValidAndEmpty(t *testing.T) {
	var a strictArgs
	if err := DecodeArgs([]byte(`{"path":"p","offset_line":2,"max_lines":3}`), &a); err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
	if a.OffsetLine != 2 || a.MaxLines != 3 {
		t.Fatalf("解码结果不符: %+v", a)
	}
	var b strictArgs
	if err := DecodeArgs(nil, &b); err != nil {
		t.Fatalf("空参数应合法: %v", err)
	}
}

// TestDecodeArgsInvalidJSONStillReported 非法 JSON 仍须报错（严格模式不得吞错）。
func TestDecodeArgsInvalidJSONStillReported(t *testing.T) {
	var a strictArgs
	err := DecodeArgs([]byte(`{"path":`), &a)
	if err == nil {
		t.Fatal("非法 JSON 必须报错")
	}
	if !errors.Is(err, toolapi.ErrInvalidArgs) {
		t.Fatalf("应包装为 ErrInvalidArgs: %v", err)
	}
}
