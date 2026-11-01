#!/usr/bin/env bash
#
# harness-go 编译脚本（只构建单二进制，不生成分发包）
# 用法：./build.sh [选项]        （Windows 直接双击 build.cmd 亦可）
#
# 选项：
#   --web        强制重建前端产物（web/dist）
#   --skip-web   跳过前端构建（只编译后端，前端缺失时 / 显示「未构建」提示页）
#   --debug      调试构建（保留符号与调试信息，不 strip）
#   -h, --help   显示本帮助
#
# 如需生成可分发/可安装的发布包（多平台 zip/tar.gz + 校验和），请用 ./package.sh
#
# 说明：本脚本与 Makefile 是等价入口，LDFLAGS 口径完全一致
#       （main.version / main.commit / main.buildTime）。

set -euo pipefail

# 无论从哪里调用，都先切到脚本所在目录（= 项目根）
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
      sed -n '3,18p' "$0" | sed 's/^# \{0,1\}//'
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
# 工具函数
# ---------------------------------------------------------------------------
say()  { echo "==> $*"; }
warn() { echo "[警告] $*" >&2; }
die()  { echo "[错误] $*" >&2; exit 1; }

human_size() {
  # 优先用 ls -lh 的可读大小；不可用时回退字节数
  ls -lh "$1" 2>/dev/null | awk '{print $5}'
}

# ---------------------------------------------------------------------------
# 1. 环境检查
# ---------------------------------------------------------------------------
command -v go >/dev/null 2>&1 || die "未找到 go，请先安装 Go 工具链：https://go.dev/dl/"

# ---------------------------------------------------------------------------
# 2. 版本信息（用于 -ldflags 注入，启动日志会打印）
#    VERSION 环境变量 > git tag > 兜底 0.1.0
# ---------------------------------------------------------------------------
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  if command -v git >/dev/null 2>&1; then
    VERSION="$(git describe --tags --abbrev=0 2>/dev/null || true)"
    VERSION="${VERSION#v}"
  fi
fi
[ -n "$VERSION" ] || VERSION="0.1.0"

COMMIT="none"
if command -v git >/dev/null 2>&1; then
  COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
  # 工作区有未提交改动时标注 -dirty，避免把「脏树构建」误当发布版本
  if [ "$COMMIT" != "none" ] && [ -n "$(git status --porcelain 2>/dev/null || true)" ]; then
    COMMIT="${COMMIT}-dirty"
  fi
fi

BUILDAT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------------------
# 3. 前端产物（web/dist）
#    铁律：web/dist 目录必须存在（//go:embed all:dist 的编译期要求），
#          由仓库内的 web/dist/.gitkeep 占位保证；vite 已配 emptyOutDir:false。
# ---------------------------------------------------------------------------
need_web=""
if [ -n "$BUILD_WEB" ]; then
  need_web=1
elif [ -n "$SKIP_WEB" ]; then
  need_web=""
elif [ ! -f web/dist/index.html ]; then
  say "未发现前端产物 web/dist/index.html，将先构建前端。"
  need_web=1
fi

# 清掉 assets 下「不被当前 index.html 引用」的历史产物。
# 原因：vite 配了 emptyOutDir:false（为保住 .gitkeep），旧 index-<hash>.js 不会自动删除，
#       实测一次改动后堆积 4 份合计 2.73MB，会被 //go:embed all:dist 全量打进二进制。
clean_stale_assets() {
  local dist="web/dist" html="web/dist/index.html"
  [ -f "$html" ] || return 0
  [ -d "$dist/assets" ] || return 0

  local keep
  keep="$(mktemp)"
  # 从 index.html 提取形如 assets/index-xxxx.js 的文件名
  grep -o 'assets/[A-Za-z0-9._-]*' "$html" 2>/dev/null | sed 's|assets/||' | sort -u >"$keep" || true

  local removed=0 f base
  for f in "$dist"/assets/*; do
    [ -f "$f" ] || continue
    base="$(basename "$f")"
    case "$base" in .*) continue ;; esac
    if ! grep -qx -- "$base" "$keep"; then
      rm -f "$f"
      removed=$((removed + 1))
    fi
  done
  rm -f "$keep"

  if [ "$removed" -gt 0 ]; then
    say "已清理 $removed 个历史前端产物（避免被 embed 打进二进制）。"
  fi
}

if [ -n "$need_web" ]; then
  command -v node >/dev/null 2>&1 || die "构建前端需要 Node.js，未找到 node：https://nodejs.org"
  command -v npm  >/dev/null 2>&1 || die "构建前端需要 npm，未找到 npm。"

  if [ ! -d web/node_modules ]; then
    say "安装前端依赖（npm install）..."
    ( cd web && npm install )
  fi

  say "构建前端产物（npm run build）..."
  ( cd web && npm run build )
else
  if [ -f web/dist/index.html ]; then
    say "复用已有前端产物（需要重建请加 --web）。"
  else
    warn "web/dist/index.html 不存在，二进制内不含前端；启动后 / 会显示「前端未构建」提示页。"
  fi
fi

clean_stale_assets

# ---------------------------------------------------------------------------
# 4. 编译 Go 单二进制
# ---------------------------------------------------------------------------
mkdir -p bin

BIN="bin/harness-go"
case "$(go env GOOS 2>/dev/null || echo unknown)" in
  windows) BIN="bin/harness-go.exe" ;;
esac

LDFLAGS="-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILDAT}"
if [ -z "$DEBUG" ]; then
  # -s -w 去符号表与 DWARF，体积通常减半；-trimpath 去掉本机绝对路径（可复现构建）
  LDFLAGS="-s -w ${LDFLAGS}"
  BUILD_PROFILE="release"
else
  BUILD_PROFILE="debug"
fi

say "编译二进制（profile: ${BUILD_PROFILE}, version: ${VERSION}, commit: ${COMMIT}）..."
go build -trimpath -ldflags "$LDFLAGS" -o "$BIN" ./cmd/harness-go

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
echo "  启动服务：./run.sh            （Windows：run.cmd）"
echo "  生成发布包：./package.sh      （Windows：package.cmd）"
