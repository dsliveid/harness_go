package main

// 离线重置口令子命令（服务端本地救命通道）。
//
// 设计立场（与 internal/api/auth_handler.go 的 change-password 严格互补）：
//   - change-password 走 HTTP，**必须**校验旧口令 —— 面向「已登录用户自助改密」；
//   - reset-password 走本地进程，**刻意不校验旧口令** —— 面向「忘记口令、连自己都登不进去」
//     的运维兜底。正因它绕过了旧口令，才**绝对不能**暴露成 HTTP 路由，否则等于给
//     任何能访问网络的人一个免旧口令的改密接口。
//
// 为什么必须做成 CLI 而不是网页上的「忘记密码」：
//   本项目是自部署单二进制，没有邮件/短信通道，任何自助重置都得先回答「怎么证明你是你」，
//   而唯一可信的凭据就是「你能读写 data 目录」。所以凭据 = 文件系统访问权，这是正确边界。

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

// resetPasswordCmd 重置口令子命令名。
const resetPasswordCmd = "reset-password"

// isResetPasswordCmd 判断本次调用是否为 reset-password 子命令。
//
// 必须在 config.Load 之前判定：config 的 flag 解析器不认识 -user/-password，
// 若把子命令参数透传进去会直接报 "flag provided but not defined"。
func isResetPasswordCmd(args []string) bool {
	return len(args) > 0 && args[0] == resetPasswordCmd
}

// runResetPassword 执行离线重置。
//
// 出错一律返回 error 由 main 打到 stderr；成功信息写 stdout（便于脚本重定向取口令）。
func runResetPassword(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(resetPasswordCmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	user := fs.String("user", "admin", "要重置的账号名")
	pwd := fs.String("password", "", "新口令（缺省随机生成 20 位并打印；显式传入会留在 shell 历史里）")
	dataDir := fs.String("data", "", "数据目录（覆盖配置文件与环境变量，默认 ./data）")
	configPath := fs.String("config", "", "配置文件路径（默认 <data>/config.yaml）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*user) == "" {
		return fmt.Errorf("账号名不能为空（用 -user 指定）")
	}

	// 复用主程序同一套配置加载（-config / HARNESS_DATA_DIR / 配置文件优先级完全一致），
	// 否则「运维以为重置了 A 目录、服务端实际读 B 目录」这类错位极难排查。
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

	// 刻意不调用 cfg.EnsureDataDir()：它会 MkdirAll，路径写错时会**新建一个空目录**
	// 然后报「账号不存在」，把「路径错了」误报成「账号没了」。先精确探测 main.db。
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

	newPwd := *pwd
	generated := false
	if newPwd == "" {
		newPwd = auth.RandomPassword(20)
		generated = true
	}

	revoked, err := svc.ResetPassword(ctx, *user, newPwd)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "✅ 已重置账号 %q 的口令（数据目录 %s）\n", *user, dir)
	if generated {
		fmt.Fprintf(stdout, "   新口令（随机生成，仅此一次显示，请立即保存）：%s\n", newPwd)
	} else {
		fmt.Fprintln(stdout, "   新口令：使用 -password 传入的值（本命令不重复回显）")
	}
	if revoked > 0 {
		fmt.Fprintf(stdout, "   已吊销该账号 %d 个现存登录令牌（其它设备的会话需重新登录）\n", revoked)
	} else {
		fmt.Fprintln(stdout, "   该账号当前没有需要吊销的登录令牌")
	}
	fmt.Fprintln(stdout, "   服务端若正在运行：无需重启，直接用新口令登录即可")
	return nil
}
