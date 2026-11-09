package main

// 本文件为 runner 模式（-mode runner）的**纯逻辑**单测。
//
// 为什么只测纯逻辑、不在这里拉起真进程：
//   - 拉起真进程需要真实桌面端二进制（单包 hd-host.exe），而它属于另一个仓库 `harness_desktop`，
//     本仓库的 `go test ./...` 不该依赖 cargo 产物（否则 clean checkout 必红）；
//   - 端到端链路由 `go run ./cmd/device-e2e -mode runner` 这个**脚本**负责验证
//     （它就是给人跑的联调工具），单测只兜住「路径探测 / URL 推导 / 输出截断」这三处
//     容易在重构中悄悄改坏、且改坏后症状极其隐蔽的逻辑。
//
// 症状为什么隐蔽：ws_url 拼错会让 Runner 连到 404；探测顺序改错会让 CI 优先用 release
// 旧产物；tailBuffer 无界会让长跑联调把内存吃光。这三处都不会「编译失败」。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWSURLForRunner_NoTokenInQuery 断言 ws_url **不带** token 查询参数。
//
// 这是本文件最重要的一条：Runner 侧（transport.rs 的 build_url）自己会附加 `?token=`。
// 若我们在 ws_url 里也拼一次，最终 URL 会是 `?token=A&token=B`，服务端取到哪个取决于
// query 解析实现 —— 表现为「偶发鉴权失败」，极难定位。
func TestWSURLForRunner_NoTokenInQuery(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"http://127.0.0.1:5611", "ws://127.0.0.1:5611/api/v1/devices/ws"},
		{"http://127.0.0.1:5611/", "ws://127.0.0.1:5611/api/v1/devices/ws"},
		{"https://example.com", "wss://example.com/api/v1/devices/ws"},
		{"ws://127.0.0.1:8080", "ws://127.0.0.1:8080/api/v1/devices/ws"},
		// 基址若已含 query（例如带追踪参数），必须被丢弃而不是叠加
		{"http://127.0.0.1:5611?x=1", "ws://127.0.0.1:5611/api/v1/devices/ws"},
	}
	for _, c := range cases {
		got, err := wsEndpointURL(c.base)
		if err != nil {
			t.Fatalf("wsEndpointURL(%q) 返回错误: %v", c.base, err)
		}
		if got != c.want {
			t.Errorf("wsEndpointURL(%q) = %q，期望 %q", c.base, got, c.want)
		}
		if strings.Contains(got, "token=") {
			t.Errorf("ws_url 不得包含 token 查询参数（Runner 会自行附加，重复会导致鉴权取错值）：%q", got)
		}
	}
}

// TestWSURLForRunner_RejectsBadScheme 非法 scheme 必须报错而不是静默产出怪 URL。
func TestWSURLForRunner_RejectsBadScheme(t *testing.T) {
	if got, err := wsEndpointURL("ftp://127.0.0.1:1"); err == nil {
		t.Fatalf("期望 ftp:// 被拒绝，实际得到 %q 且无错误", got)
	}
}

// TestDetectRunnerBinary_ExplicitPath 显式指定优先，且必须真的校验「存在 + 非目录 + 非空」。
func TestDetectRunnerBinary_ExplicitPath(t *testing.T) {
	dir := t.TempDir()

	// ① 正常文件：应当直接返回其绝对路径
	good := filepath.Join(dir, exeName())
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	got, _, err := detectRunnerBinary(good)
	if err != nil {
		t.Fatalf("显式指定有效路径应当成功，实际报错: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("返回路径应为绝对路径，实际 %q", got)
	}

	// ② 空文件：cargo 构建中途失败会留下 0 字节产物，这时必须判「不可用」，
	//    否则我们会拉起一个必然失败的进程，把「没编译完」误报成「协议不兼容」。
	empty := filepath.Join(dir, "empty-"+exeName())
	if err := os.WriteFile(empty, nil, 0o755); err != nil {
		t.Fatalf("写空文件失败: %v", err)
	}
	if _, _, err := detectRunnerBinary(empty); err == nil {
		t.Errorf("0 字节的文件必须被判为不可用")
	}

	// ③ 目录：用户常把 -runner-bin 指到 target/debug 这个目录上
	if _, _, err := detectRunnerBinary(dir); err == nil {
		t.Errorf("目录必须被判为不可用")
	}

	// ④ 不存在
	if _, _, err := detectRunnerBinary(filepath.Join(dir, "nope", exeName())); err == nil {
		t.Errorf("不存在的路径必须报错")
	}
}

// TestDetectRunnerBinary_MissingReportsCandidates 探测失败时**必须**把试过的位置都带回来。
//
// 理由：用户只知道「找不到」是没法自查的（他本来就觉得应该有）。带回候选清单后，
// 脚本能直接打印「我看过这 4 个地方」，排查成本从「猜」降到「看一眼」。
func TestDetectRunnerBinary_MissingReportsCandidates(t *testing.T) {
	t.Setenv("HD_RUNNER_BIN", "")
	// 用一个几乎不可能存在的显式路径不会被接受检查，故这里走自动探测分支：
	// 自动探测基于工作区相对路径，测试环境下通常不存在 —— 若存在（开发机已构建），
	// 则该断言自动放宽（这种环境依赖不应让单测变红）。
	bin, cands, err := detectRunnerBinary("")
	if err == nil {
		// 开发机上已构建出二进制：只校验候选清单存在且能定位到命中项
		if len(cands) == 0 {
			t.Fatalf("探测成功但候选清单为空")
		}
		r := reasonOf(cands, bin)
		if r == "" {
			t.Errorf("探测成功时应能说明「从哪个候选命中」")
		}
		t.Skipf("本机已存在 %s，跳过「缺失」分支断言", bin)
	}
	if len(cands) == 0 {
		t.Fatalf("探测失败时也必须返回候选清单（否则用户无法自查）")
	}
	for _, c := range cands {
		if c.Reason == "" {
			t.Errorf("候选 %q 缺少 Reason 说明", c.Path)
		}
	}
	// 单包改造断言：自动探测的候选里必须出现 hd-host 文件名（路径已由 detectRunnerBinary 组装）。
	sawHost := false
	for _, c := range cands {
		base := filepath.Base(c.Path)
		if base == "hd-host" || base == "hd-host.exe" {
			sawHost = true
		}
		if strings.HasPrefix(base, "hd-runner") {
			t.Errorf("候选 %q 仍指向已废弃的 hd-runner（单包分发后只有 hd-host）", c.Path)
		}
	}
	if !sawHost {
		t.Errorf("候选清单里没有任何 hd-host 路径，排查方向会误导用户：%+v", cands)
	}
}

// TestTailBuffer_KeepsTail 只保留尾部（错误现场在末尾，而不是开头）。
func TestTailBuffer_KeepsTail(t *testing.T) {
	tb := newTailBuffer(16)
	if _, err := tb.Write([]byte("AAAAAAAAAAAAAAAA")); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Write([]byte("BBBB")); err != nil {
		t.Fatal(err)
	}
	got := tb.String()
	if len(got) != 16 {
		t.Fatalf("应当只保留 16 字节，实际 %d 字节（%q）", len(got), got)
	}
	if !strings.HasSuffix(got, "BBBB") {
		t.Errorf("尾部内容必须被保留（最后写入的字节不能丢），实际 %q", got)
	}
	// 裁剪必须发生在**头部**：保留的最后 16 字节是 12 个 A + 4 个 B。
	if got != "AAAAAAAAAAAABBBB" {
		t.Errorf("应保留尾部 16 字节（12×A + 4×B），实际 %q", got)
	}
}

// TestRunnerBuildHint_IsActionable 提示必须是**可直接照抄执行**的命令，而不是「请自行编译」。
//
// ⚠️ 单包形态：命令必须指向 hd-host（带 --features gui），并显式说明自动追加 --runner，
// 否则用户照抄旧命令 `cargo build -p hd-runner` 只会得到「package not found」。
func TestRunnerBuildHint_IsActionable(t *testing.T) {
	hint := runnerBuildHint()
	for _, want := range []string{"cargo build", "hd-host", "--features gui", "hd-runner", "harness_desktop", "--runner"} {
		if !strings.Contains(hint, want) {
			t.Errorf("构建提示缺少关键信息 %q：%s", want, hint)
		}
	}
}

// TestRunnerExeNames_SinglePackage 单包分发后探测的文件名必须只有 hd-host（不再有 hd-runner）。
//
// 为什么值得单测：文件名写错不会编译失败，只会让 -mode runner 永远 SKIP，
// 而 SKIP 是「静默降级」—— 很容易被误读成「本机没构建」而长期无人察觉。
func TestRunnerExeNames_SinglePackage(t *testing.T) {
	names := runnerExeNames()
	if len(names) == 0 {
		t.Fatal("候选文件名不能为空")
	}
	if names[0] != exeName() {
		t.Errorf("首个候选应为首选文件名 %q，实际 %q", exeName(), names[0])
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "hd-host") {
			t.Errorf("候选文件名 %q 不是 hd-host 系列（单包分发后 hd-runner 已不存在）", n)
		}
	}
	if runtime.GOOS == "windows" {
		// Windows 首项必须带 .exe；同时保留不带后缀的兼容尝试。
		if names[0] != "hd-host.exe" {
			t.Errorf("Windows 首个候选必须带 .exe，实际 %q", names[0])
		}
		if len(names) < 2 || names[1] != "hd-host" {
			t.Errorf("Windows 应保留不带后缀的兼容尝试，实际 %v", names)
		}
	}
}

// TestBuildRunnerArgs_RunnerFlagAlwaysFirst 这是单包改造的核心不变量：
// 拉起 hd-host 时**必须**带 `--runner` 且放在第一个参数位。
//
// 丢了它不会编译失败，也不会立刻报错 —— 进程会以 GUI 宿主身份起来（开窗口、不读 stdin），
// 症状是「联调卡在等待设备上线 20 秒后超时」，从现象几乎无法反推到参数缺失。故用单测钉死。
func TestBuildRunnerArgs_RunnerFlagAlwaysFirst(t *testing.T) {
	for _, withDev := range []bool{false, true} {
		args := buildRunnerArgs(withDev)
		if len(args) == 0 || args[0] != "--runner" {
			t.Fatalf("withDev=%v：首个参数必须是 --runner，实际 %v", withDev, args)
		}
		for _, a := range args {
			if a == "--runner" {
				continue
			}
			if a != "--dev" {
				t.Errorf("withDev=%v：出现未知参数 %q（%v）", withDev, a, args)
			}
		}
		if withDev {
			// --runner 与 --dev 必须共存，且 --runner 在前。
			if len(args) != 2 || args[1] != "--dev" {
				t.Errorf("withDev=true 时应为 [--runner --dev]，实际 %v", args)
			}
		} else if len(args) != 1 {
			t.Errorf("withDev=false 时应只有 [--runner]，实际 %v", args)
		}
	}
	// 命中数断言：保证 --runner 只出现一次（重复传参在部分 CLI 解析器下行为未定义）。
	args := buildRunnerArgs(true)
	n := 0
	for _, a := range args {
		if a == "--runner" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("--runner 必须恰好出现一次，实际 %d 次（%v）", n, args)
	}
}

// TestMaskToken_NeverLeaksFull 令牌打码是安全红线，必须逐字符确认不会漏出完整值。
func TestMaskToken_NeverLeaksFull(t *testing.T) {
	tok := "hgd_0123456789abcdef"
	got := maskToken(tok)
	if got == tok || strings.Contains(got, "6789abcdef") {
		t.Fatalf("令牌被完整泄露：%q", got)
	}
	if !strings.HasPrefix(got, "hgd_") {
		t.Errorf("打码后应保留可辨识前缀，实际 %q", got)
	}
	if maskToken("") != "(空)" {
		t.Errorf("空令牌应给出明确标识")
	}
}

// exeName 测试用：与 runnerExeNames 的首项保持一致。
//
// ⚠️ 单包分发后首项是 hd-host[.exe]（旧的 hd-runner.exe 已不再产出）。
func exeName() string {
	if runtime.GOOS == "windows" {
		return "hd-host.exe"
	}
	return "hd-host"
}
