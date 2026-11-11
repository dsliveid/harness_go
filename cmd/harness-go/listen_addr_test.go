package main

// 监听地址判定单测（无外部依赖，可随 go test ./... 直接跑）。
//
// 背景（2026-11 实测事故）：用户反馈「服务端只支持本地访问，用局域网 IP 打不开」。
// 实测 netstat 显示 `TCP 127.0.0.1:5611 LISTENING` —— 默认 Addr 绑的是回环网卡，
// 内核不会为局域网网卡建监听，故 http://<本机IP>:5611 必然连接超时（curl 返回 000）。
//
// isLoopbackOnly 报告监听地址是否只绑定回环网卡。
//
// 语义方向（2026-11 起随默认值翻转）：默认监听已是 0.0.0.0:5611（局域网开箱可访问），
// 检测到仅回环时说明用户**显式收窄**了范围（-addr 127.0.0.1:N 等），此时打 Info 说明。
// （判定逻辑本身不变：仍须与 net.Listen 的真实行为严格一致——一旦误判，提示就变成误导。）

import "testing"

func TestIsLoopbackOnly(t *testing.T) {
	cases := []struct {
		addr string
		want bool
		why  string
	}{
		// ---- 仅本机：只绑定回环网卡 ----
		{"127.0.0.1:5611", true, "默认值：绑 IPv4 回环"},
		{"localhost:5611", true, "主机名 localhost 等价回环"},
		{"[::1]:5611", true, "IPv6 回环"},
		{"127.0.0.2:5611", true, "127.0.0.0/8 整段都是回环（不是只有 .1）"},

		// ---- 对外开放：绑全部网卡 ----
		{"0.0.0.0:5611", false, "绑全部 IPv4 网卡"},
		{"[::]:5611", false, "绑全部 IPv6 网卡"},
		{":5611", false, "空 host 等同绑全部网卡"},
		{"192.168.226.121:5611", false, "绑具体局域网网卡"},

		// ---- 只写端口：net.Listen 语义等同 \":5611\" ----
		{"5611", false, "纯端口等同绑全部网卡"},
	}

	for _, c := range cases {
		if got := isLoopbackOnly(c.addr); got != c.want {
			t.Errorf("isLoopbackOnly(%q) = %v, want %v（%s）", c.addr, got, c.want, c.why)
		}
	}
}

// TestIsLoopbackOnlyGarbage 非法地址必须保守返回 true。
//
// 取舍：非法/无法静态判定的地址宁可多提示一次「仅本机可访问」，
// 也不能让用户误以为服务已经对外开放（后者会让人以为防火墙或路由有问题而白折腾）。
func TestIsLoopbackOnlyGarbage(t *testing.T) {
	for _, addr := range []string{"", "not-an-addr", "example.com:5611", ":::"} {
		if !isLoopbackOnly(addr) {
			t.Errorf("非法/未知地址 %q 应保守判为仅回环，实际返回 false", addr)
		}
	}
}
