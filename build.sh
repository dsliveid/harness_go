#!/usr/bin/env bash
#
# harness-go 后端编译脚本（只构建本机单二进制 bin/，不生成分发包）
# 用法：./build.sh [选项]        （Windows：build.cmd）
#
# 选项：
#   --web        强制重建前端产物（委托 web/build.sh）
#   --skip-web   跳过前端构建（只编译后端；前端缺失时 / 显示「未构建」提示页）
#   --debug      调试构建（保留符号与调试信息，不 strip）
#   -h, --help   显示本帮助
#
# 产物：bin/harness-go[.exe]（前端 web/dist 在编译期被 //go:embed 内嵌）
#
# 与 package.sh 的分工（两者不重复，是"日常"与"发版"的关系）：
#   build.sh    本机平台 ×1，秒级，产出 bin/，供 run.sh 启动与本地调试
#   package.sh  多平台交叉编译 → dist/ 分发包目录 + zip/tar.gz + SHA256SUMS（+可选 NSIS）
# 公共逻辑（版本信息 / LDFLAGS / 前端构建）已抽到 scripts/lib/common.sh 与 web/build.sh，
# 故两者不会各自维护一份而漂移。
#
# 说明：本脚本与 Makefile 的 build 目标等价，LDFLAGS 口径一致
#       （main.version / main.commit / main.buildTime）。

set -euo pipefail

# 无论从哪里调用，都先切到脚本所在目录（= 项目根）
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

# 公共库：say / warn / die / human_size / harness_versions / harness_ldflags
#         / harness_bin_path / ensure_web
. "$ROOT/scripts/lib/common.sh"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
BUILD_WEB=""
SKIP_WEB=""
DEBUG=""

for arg in "$@"; do
  case "$arg" in
    --web)      BUILD_WEB=1 ;;
    --skip-web) SKIP_WEB=1 ;;
    --debug)    DEBUG=1 ;;
    -h|--help)
      sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "错误：未知参数 $arg（可用：--web / --skip-web / --debug）" >&2
      exit 2
      ;;
  esac
done

if [ -n "$BUILD_WEB" ] && [ -n "$SKIP_WEB" ]; then
  echo "错误：--web 与 --skip-web 不能同时使用。" >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# 1. 环境检查
# ---------------------------------------------------------------------------
command -v go >/dev/null 2>&1 || die "未找到 go，请先安装 Go 工具链：https://go.dev/dl/"

# ---------------------------------------------------------------------------
# 2. 前端产物（唯一构建入口：web/build.sh，本脚本不再内联 npm 逻辑）
#    铁律：web/dist 目录必须存在（//go:embed all:dist 的编译期要求），
#          由仓库内的 web/dist/.gitkeep 占位保证；vite 已配 emptyOutDir:false。
# ---------------------------------------------------------------------------
if [ -n "$BUILD_WEB" ]; then
  ensure_web force || die "前端构建失败，已中止（如只想编译后端请加 --skip-web）。"
elif [ -n "$SKIP_WEB" ]; then
  ensure_web skip
else
  # 按需：dist 缺失或前端源码更新时重建（源码比产物新同样要重建，否则内嵌的是旧 UI）
  ensure_web || die "前端构建失败，已中止（如只想编译后端请加 --skip-web）。"
fi

# ---------------------------------------------------------------------------
# 3. 版本信息 + LDFLAGS（统一由 common.sh 生成，避免与 package.sh 漂移）
# ---------------------------------------------------------------------------
harness_versions
harness_ldflags "$DEBUG"

# ---------------------------------------------------------------------------
# 4. 编译 Go 单二进制
# ---------------------------------------------------------------------------
mkdir -p bin
BIN="$(harness_bin_path)"

say "编译二进制（profile: ${HARNESS_PROFILE}, version: ${HARNESS_VERSION}, commit: ${HARNESS_COMMIT}）..."
go build -trimpath -ldflags "$HARNESS_LDFLAGS" -o "$BIN" ./cmd/harness-go

# ---------------------------------------------------------------------------
# 5. 列出产物
# ---------------------------------------------------------------------------
echo
echo "==> 构建完成，产物："
echo "  可执行文件  ${BIN}  ($(human_size "$BIN"))"
if [ -f web/dist/index.html ]; then
  echo "  前端产物    web/dist（已内嵌进二进制）"
else
  echo "  前端产物    无（未构建，/ 将显示提示页）"
fi
echo
echo "  启动服务：  ./run.sh                 （Windows：run.cmd）"
echo "  前端热更新：./web/run.sh             （Windows：web\\run.cmd）"
echo "  生成发布包：./package.sh             （Windows：package.cmd）"
