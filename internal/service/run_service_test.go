package service

import "testing"

// 自动标题口径（对齐桌面端 commands.rs：首条消息前 24 字，仅默认标题时生效）。
func TestDeriveSessionTitle(t *testing.T) {
	cases := []struct {
		name    string
		current string
		input   string
		want    string
	}{
		{"默认标题+普通输入", "新对话", "帮我看看这段代码为什么报错", "帮我看看这段代码为什么报错"},
		{"空标题+普通输入", "", "短输入", "短输入"},
		{"超长输入截断 24 字", "新对话", "这是一段非常非常非常非常非常非常非常非常长的输入内容已经超过二十四个字符了", "这是一段非常非常非常非常非常非常非常非常长的输入"},
		{"自定义标题不覆盖", "我的重要讨论", "新消息", ""},
		{"空白输入不生成", "新对话", "   \n\t ", ""},
		{"首尾空白被修剪", "新对话", "  带空白的输入  ", "带空白的输入"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveSessionTitle(c.current, c.input)
			if got != c.want {
				t.Fatalf("deriveSessionTitle(%q, %q) = %q, want %q", c.current, c.input, got, c.want)
			}
		})
	}
}
