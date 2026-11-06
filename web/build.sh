#!/usr/bin/env bash
#
# harness-go 前端构建脚本（仅构建前端产物 web/dist，不碰 Go）
# 用法：./web/build.sh [选项]        （Windows：web\build.cmd）
#
# 选项：
#   --install    即使 node_modules 已存在也重装依赖（npm install）
#   --ci         用 npm ci 代替 npm install（依赖 package-lock.json，可复现）
#   --keep-stale 不清理 web/dist/assets 下不被当前 index.html 引用的历史产物
#   -h, --help   显示本帮助
#
# 产物：web/dist/（index.html + assets/*）
#   该目录随后会被 Go 通过 //go:embed all:dist 在**编译期**打进二进制，
#   所以改完前端必须重新构建前端 + 重新编译后端，二者缺一不可。
#
# 铁律：web/dist 目录必须存在（//go:embed all:dist 的编译期要求），
#       由仓库内的 web/dist/.gitkeep 占位保证；vite 已配 emptyOutDir:false 不删它。

set -euo pipefail

# 无论从哪里调用，都切到**项目根**（本脚本位于 web/ 下，故上一级即根）
WEB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$WEB_DIR/.." && pwd)"
cd "$ROOT"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
DO_INSTALL=""
USE_CI=""
KEEP_STALE=""

for arg in "$@"; do
  case "$arg" in
    --install)    DO_INSTALL=1 ;;
    --ci)         USE_CI=1 ;;
    --keep-stale) KEEP_STALE=1 ;;
    -h|--help)
      sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "错误：未知参数 $arg（可用：--install / --ci / --keep-stale）" >&2
      exit 2
      ;;
  esac
done

say()  { echo "==> $*"; }
die()  { echo "[错误] $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 1. 环境检查（Node 仅前端构建需要，后端脚本不再各自检查一遍）
# ---------------------------------------------------------------------------
command -v node >/dev/null 2>&1 || die "构建前端需要 Node.js，未找到 node：https://nodejs.org"
command -v npm  >/dev/null 2>&1 || die "构建前端需要 npm，未找到 npm。"

# ---------------------------------------------------------------------------
# 2. 依赖
# ---------------------------------------------------------------------------
if [ -n "$USE_CI" ]; then
  say "安装前端依赖（npm ci）..."
  ( cd web && npm ci )
elif [ -n "$DO_INSTALL" ] || [ ! -d web/node_modules ]; then
  say "安装前端依赖（npm install）..."
  ( cd web && npm install )
fi

# ---------------------------------------------------------------------------
# 3. 构建（tsc -b && vite build，见 web/package.json 的 build 脚本）
#
# 必须走 npm run build 而不是直接 npx vite build：
#   tsc -b 做类型检查（含 tsconfig.node.json 的 composite 引用），vite build 只做打包。
#   ⚠️ 门禁盲区：tsconfig.json 的 include 仅含 src，vite.config.ts 不在 tsc 覆盖内，
#      其语法错误只在 vite build 的 esbuild 阶段暴露 → 两步都不能省。
# ---------------------------------------------------------------------------
say "构建前端产物（npm run build）..."
( cd web && npm run build )

# ---------------------------------------------------------------------------
# 4. 清理历史产物
#    原因：vite 配了 emptyOutDir:false（为保住 .gitkeep），旧 index-<hash>.js 不会自动删除，
#          实测一次改动后堆积 4 份合计 2.73MB，会被 //go:embed all:dist 全量打进二进制。
# ---------------------------------------------------------------------------
prune_stale_assets() {
  local dist="web/dist" html="web/dist/index.html"
  [ -f "$html" ] || return 0
  [ -d "$dist/assets" ] || return 0

  local keep
  keep="$(mktemp)"
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
  return 0
}

if [ -n "$KEEP_STALE" ]; then
  say "跳过历史产物清理（--keep-stale）。"
else
  prune_stale_assets
fi

# ---------------------------------------------------------------------------
# 5. 结果
# ---------------------------------------------------------------------------
[ -f web/dist/index.html ] || die "构建结束但未生成 web/dist/index.html，请检查上方 npm 输出。"

echo
say "前端构建完成：web/dist/index.html（$(ls -lh web/dist/index.html | awk '{print $5}')）"
echo "  下一步（要生效必须重编后端，前端产物是编译期内嵌的）："
echo "    ./build.sh --skip-web     只编译后端并复用本次产物"
echo "    ./run.sh --dev            开发模式直接 go run"
echo "    ./package.sh --no-web     打发布包并复用本次产物"
