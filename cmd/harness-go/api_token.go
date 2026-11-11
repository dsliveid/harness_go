package main

// API 令牌的落盘与轮转子命令。
//
// 背景（真实事故）：首启引导令牌曾用 logger.Warn(..., "token", plain) 打印，
// 而 logger 是 io.MultiWriter(os.Stdout, data/app.log) —— 明文令牌直接落到磁盘，
// GET /api/v1/logs（admin scope）能在界面上原样回读，轮转后还有 app.log.1..N 多份。
// 「仅显示一次」的语义在日志文件面前完全不成立。
//
// 本文件的两件事，正是这条事故的两半修复：
//  1. persistBootstrapToken：新令牌只写 0600 文件，日志只出现文件路径；
//  2. rotate-api-token 子命令：为「已经泄漏过的历史令牌」提供确定性作废手段。
//     只吊销单条做不到——明文在日志里、hash 在库内，无法反查 id，所以按 kind 整体吊销。

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"harness-go/internal/auth"
	"harness-go/internal/config"
	"harness-go/internal/store"
)

// initialTokenFile 首启 API 令牌文件名（位于 data 目录下，权限 0600）。
const initialTokenFile = "initial_token"

// rotateAPITokenCmd 轮转 API 令牌子命令名。
const rotateAPITokenCmd = "rotate-api-token"

// isRotateAPITokenCmd 判断本次调用是否为 rotate-api-token 子命令。
//
// 与 reset-password 同口径：必须在 config.Load 之前判定，否则 -data/-config 之外的
// 子命令参数会被 config 的 flag 解析器当成未知参数直接报错。
func isRotateAPITokenCmd(args []string) bool {
	return len(args) > 0 && args[0] == rotateAPITokenCmd
}

// persistBootstrapToken 把令牌明文写入 <dataDir>/initial_token（0600），返回文件路径。
//
// 铁律：**任何失败路径都不得把明文打印/记录到日志**。写入失败时宁可让令牌「本次不可见」
// （调用方可用 rotate-api-token 重新签发），也不退化成打印明文兜底——那等于把事故恢复原状。
func persistBootstrapToken(dataDir, plain string) (string, error) {
	if strings.TrimSpace(plain) == "" {
		return "", fmt.Errorf("令牌明文为空")
	}
	path := filepath.Join(dataDir, initialTokenFile)
	// 写成 curl 可直接 -H @file 使用的形态，避免用户手工拼 Bearer 前缀出错。
	content := "Authorization: Bearer " + plain + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return path, err
	}
	return path, nil
}

// runRotateAPIToken 作废现有全部 api 令牌并签发新的，写入 data/initial_token。
//
// 与 reset-password 严格同构：走本地进程 + 文件系统访问权即凭据，**不暴露成 HTTP 路由**
// （否则任何能访问网络的人都能让服务端令牌一夜全废，属于拒绝服务面）。
func runRotateAPIToken(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(rotateAPITokenCmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", "", "数据目录（覆盖配置文件与环境变量，默认 ./data）")
	configPath := fs.String("config", "", "配置文件路径（默认 <data>/config.yaml）")
	show := fs.Bool("print", false, "把新令牌明文打印到 stdout（默认只写文件；有终端留痕风险，慎用）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 复用主程序同一套配置加载，避免「重置了 A 目录、服务端实际读 B 目录」的错位。
	var cfgArgs []string
	if *configPath != "" {
		cfgArgs = append(cfgArgs, "-config", *configPath)
	}
	if *dataDir != "" {
		cfgArgs = append(cfgArgs, "-data", *dataDir)
	}
	cfg, err := config.Load(config.Options{Args: cfgArgs})
	if err != nil {
		return err
	}

	// 与 reset-password 同口径：不调用 EnsureDataDir，先精确探测 main.db，
	// 避免路径写错时新建空目录并误报「没有需要吊销的令牌」。
	dir, err := filepath.Abs(cfg.Data.Dir)
	if err != nil {
		return fmt.Errorf("解析数据目录失败: %w", err)
	}
	dbPath := filepath.Join(dir, "main.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return fmt.Errorf("数据目录 %s 下找不到 main.db（%v）。\n"+
			"请用 -data <数据目录> 指定服务端实际使用的目录（默认 ./data）", dir, statErr)
	}

	ctx := context.Background()
	mainStore, err := store.OpenMain(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = mainStore.Close() }()

	svc := auth.NewService(auth.Options{Main: mainStore})
	plain, revoked, err := svc.RotateBootstrapToken(ctx)
	if err != nil {
		return err
	}

	path, wErr := persistBootstrapToken(dir, plain)
	fmt.Fprintf(stdout, "✅ 已轮转 API 令牌（数据目录 %s）\n", dir)
	fmt.Fprintf(stdout, "   已作废 %d 个旧 api 令牌（网页会话/SSO 会话不受影响）\n", revoked)
	if wErr != nil {
		// 文件写不进去时，唯一能让用户拿到令牌的通道就是 stdout——此时明确告知并给出手工落盘建议，
		// 但仍不写日志（stdout 是用户主动执行命令的即时输出，与被持久化的 app.log 风险等级不同）。
		fmt.Fprintf(stdout, "   ⚠️ 写入 %s 失败：%v\n", path, wErr)
		if *show {
			fmt.Fprintf(stdout, "   新令牌（仅此一次显示，请立即保存）：%s\n", plain)
		} else {
			fmt.Fprintln(stdout, "   令牌本次未落盘，重新执行并加 -print 可取回明文")
		}
		return nil
	}
	fmt.Fprintf(stdout, "   新令牌已写入 %s（权限 0600），可直接用于 curl -H @%s\n", path, path)
	if *show {
		fmt.Fprintf(stdout, "   新令牌（显式 -print，仅此一次显示）：%s\n", plain)
	}
	fmt.Fprintln(stdout, "   服务端若正在运行：无需重启，旧令牌立即失效、新令牌立即生效")
	return nil
}
