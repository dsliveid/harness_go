#!/usr/bin/env bash
#
# harness-go **demo 原型**启动脚本（纯前端，不连后端）
# 用法：./demo/run.sh [选项] [-- 透传给 vite 的参数...]   （Windows：demo\run.cmd）
#
# 定位：demo/ 是早期的纯前端原型，数据全部来自 demo/src/mock，**与后端无关**。
#   正式前端在 web/（入口 web/run.sh），两者代码零引用、互不影响。
#
# 选项：
#   --port N      vite dev server 端口（默认 5610；被占用时自动顺延 +1）
#   --build       先 npm run build 产出 demo/dist，再以预览模式启动
#   --preview     不跑 vite dev，直接用零依赖 serve.cjs 托管已有的 demo/dist
#   --install     强制重装依赖
#   --open        启动后自动打开浏览器（仅 vite dev 模式有效）
#   --dry-run     只打印将要执行的命令
#   -h, --help    显示本帮助
#
# 端口分工（三者互不撞号，勿随意改）：
#   5610 本脚本（demo 原型，纯前端）
#   5611 后端 harness-go
#   5612 web/run.sh 的 vite dev server
#
# ⚠️ 原型不连后端：页面里的数据全部是 demo/src/mock 的样例，与 5611 上的真实服务无关。
#    需要看真实数据请改用正式前端：./web/run.sh（Windows：web\run.cmd）

set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$DEMO_DIR/.." && pwd)"

# 复用公共库的日志函数，保持与其他脚本一致的输出风格（只取 say/warn/die）
if [ -f "$ROOT/scripts/lib/common.sh" ]; then
  . "$ROOT/scripts/lib/common.sh"
fi
command -v say  >/dev/null 2>&1 || say()  { echo "==> $*"; }
command -v warn >/dev/null 2>&1 || warn() { echo "[警告] $*" >&2; }
command -v die  >/dev/null 2>&1 || die()  { echo "[错误] $*" >&2; exit 1; }

cd "$ROOT"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
PORT="5610"
DO_BUILD=""
DO_PREVIEW=""
DO_OPEN=""
DO_INSTALL=""
DRY_RUN=""
EXTRA=()
PASSTHROUGH=""

while [ $# -gt 0 ]; do
  case "$1" in
    --port)    PORT="${2:-}"; shift ;;
    --port=*)  PORT="${1#*=}" ;;
    --build)   DO_BUILD=1 ;;
    --preview) DO_PREVIEW=1 ;;
    --open)    DO_OPEN=1 ;;
    --install) DO_INSTALL=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --)        shift; PASSTHROUGH=1 ;;
    -h|--help)
      sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      if [ -n "$PASSTHROUGH" ]; then
        EXTRA+=("$1")
      else
        echo "错误：未知参数 $1（可用：--port / --build / --preview / --install / --open / --dry-run）" >&2
        exit 2
      fi
      ;;
  esac
  shift
done

case "$PORT" in
  ''|*[!0-9]*) die "--port 需要数字，收到：'$PORT'" ;;
esac

command -v node >/dev/null 2>&1 || die "启动 demo 原型需要 Node.js，未找到 node：https://nodejs.org"
command -v npm  >/dev/null 2>&1 || die "启动 demo 原型需要 npm。"

# ---------------------------------------------------------------------------
# 依赖（demo 有自己的 node_modules，与 web 完全独立）
# ---------------------------------------------------------------------------
if [ -n "$DO_INSTALL" ] || [ ! -d demo/node_modules ]; then
  say "安装 demo 依赖（npm install）..."
  [ -n "$DRY_RUN" ] || ( cd demo && npm install )
fi

# ---------------------------------------------------------------------------
# 构建（可选）：--build 或 --preview 但缺少产物时自动构建
# ---------------------------------------------------------------------------
if [ -n "$DO_BUILD" ]; then
  say "构建 demo 产物（tsc -b && vite build）..."
  [ -n "$DRY_RUN" ] || ( cd demo && npm run build )
elif [ -n "$DO_PREVIEW" ] && [ ! -f demo/dist/index.html ]; then
  say "未发现 demo/dist/index.html，先构建 demo 产物..."
  [ -n "$DRY_RUN" ] || ( cd demo && npm run build )
fi

URL="http://127.0.0.1:${PORT}"

echo
if [ -n "$DO_PREVIEW" ]; then
  # 零依赖静态托管：node serve.cjs [port] [host]
  # host 传 127.0.0.1，避免原型默认监听 0.0.0.0 暴露到局域网
  say "demo 原型（静态预览，托管 demo/dist）"
  echo "  地址      ${URL}"
  echo "  说明      纯前端原型，数据来自 demo/src/mock，不连后端"
  echo "  退出：Ctrl+C"
  echo
  if [ -n "$DRY_RUN" ]; then
    say "dry-run：以下命令未真正执行"
    echo "  (cd demo && node serve.cjs $PORT 127.0.0.1)"
    exit 0
  fi
  cd demo
  exec node serve.cjs "$PORT" 127.0.0.1
fi

# 默认：vite dev（读 demo/src 源码，HMR，永远最新）
VITE_ARGS=(--port "$PORT")
[ -n "$DO_OPEN" ] && VITE_ARGS+=(--open)
if [ "${#EXTRA[@]}" -gt 0 ]; then
  VITE_ARGS+=("${EXTRA[@]}")
fi

say "demo 原型（vite dev，HMR）"
echo "  地址      ${URL}"
echo "  说明      纯前端原型，数据来自 demo/src/mock，不连后端"
echo "  对照      正式前端：./web/run.sh（端口 5612，连后端 5611）"
echo "  退出：Ctrl+C"
echo

if [ -n "$DRY_RUN" ]; then
  say "dry-run：以下命令未真正执行"
  echo "  (cd demo && npm run dev -- ${VITE_ARGS[*]})"
  exit 0
fi

# ---------------------------------------------------------------------------
# 启动（exec 让 vite 成为前台进程，Ctrl+C 直接可达，不留孤儿进程）
# ---------------------------------------------------------------------------
cd demo
exec npm run dev -- "${VITE_ARGS[@]}"
