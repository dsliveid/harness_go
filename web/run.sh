#!/usr/bin/env bash
#
# harness-go 前端启动脚本（vite dev server，热更新 HMR；不启动后端）
# 用法：./web/run.sh [选项] [-- 透传给 vite 的参数...]   （Windows：web\run.cmd）
#
# 选项：
#   --port N      vite dev server 端口（默认 5612；被占用时自动顺延 +1）
#   --real        直连真实后端（**默认即此模式**，等价于 npm run dev:real，加载 .env.real
#                 注入 VITE_API_MODE=real）
#   --backend URL 后端地址（注入 HARNESS_BACKEND，覆盖 vite proxy 默认的
#                 http://127.0.0.1:5611）
#   --install     强制重装依赖
#   --open        启动后自动打开浏览器
#   --dry-run     只打印将要执行的命令
#   -h, --help    显示本帮助
#
# 端口分工（三者互不撞号，勿随意改）：
#   5610 demo 原型（入口：demo/run.sh，纯前端、不连后端）
#   5611 后端 harness-go
#   5612 本脚本的 vite dev server
#
# ⚠️ 本脚本只起前端 dev server（读 web/src 源码，永远是最新页面）。
#   若要看**后端 5611 托管的页面**，那份页面来自 `//go:embed web/dist` 的编译期快照，
#   必须先在 web/ 下 `npm run build` 重新产出 dist 再重启后端，否则看到的是旧产物。
# ⚠️ 后端须另开终端（默认 5611）：./run.sh --dev --port 5611
#   vite 会把 /api 与 /healthz 经 proxy 转发给后端（见 web/vite.config.ts）。

set -euo pipefail

WEB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$WEB_DIR/.." && pwd)"
cd "$ROOT"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
PORT="5612"
MODE="real"      # 恒为 real：本脚本只服务正式前端，演示原型见 demo/run.sh
BACKEND=""
DO_OPEN=""
DO_INSTALL=""
DRY_RUN=""
EXTRA=()
PASSTHROUGH=""

while [ $# -gt 0 ]; do
  case "$1" in
    --port)    PORT="${2:-}"; shift ;;
    --port=*)  PORT="${1#*=}" ;;
    --real)    MODE="real" ;;   # 兼容旧习惯的显式写法（默认已是 real）
    --backend) BACKEND="${2:-}"; shift ;;
    --backend=*) BACKEND="${1#*=}" ;;
    --open)    DO_OPEN=1 ;;
    --install) DO_INSTALL=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --)        shift; PASSTHROUGH=1 ;;
    -h|--help)
      sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      if [ -n "$PASSTHROUGH" ]; then
        EXTRA+=("$1")
      else
        echo "错误：未知参数 $1（可用：--port / --real / --backend / --install / --open / --dry-run）" >&2
        exit 2
      fi
      ;;
  esac
  shift
done

say()  { echo "==> $*"; }
warn() { echo "[警告] $*" >&2; }
die()  { echo "[错误] $*" >&2; exit 1; }

case "$PORT" in
  ''|*[!0-9]*) die "--port 需要数字，收到：'$PORT'" ;;
esac

command -v node >/dev/null 2>&1 || die "启动前端 dev server 需要 Node.js，未找到 node：https://nodejs.org"
command -v npm  >/dev/null 2>&1 || die "启动前端 dev server 需要 npm。"

# ---------------------------------------------------------------------------
# 依赖
# ---------------------------------------------------------------------------
if [ -n "$DO_INSTALL" ] || [ ! -d web/node_modules ]; then
  say "安装前端依赖（npm install）..."
  [ -n "$DRY_RUN" ] || ( cd web && npm install )
fi

# ---------------------------------------------------------------------------
# 组装命令
# ---------------------------------------------------------------------------
NPM_SCRIPT="dev"
[ "$MODE" = "real" ] && NPM_SCRIPT="dev:real"

# vite CLI 参数：--port 走 CLI 最稳（strictPort:false，被占用自动顺延）
VITE_ARGS=(--port "$PORT")
[ -n "$DO_OPEN" ] && VITE_ARGS+=(--open)
if [ "${#EXTRA[@]}" -gt 0 ]; then
  VITE_ARGS+=("${EXTRA[@]}")
fi

export HARNESS_BACKEND="${BACKEND:-}"
[ -n "$BACKEND" ] || unset HARNESS_BACKEND

URL="http://127.0.0.1:${PORT}"

echo
say "前端 dev server（vite HMR）"
echo "  地址      ${URL}"
echo "  数据模式  real（直连真实后端）"
echo "  后端代理  ${HARNESS_BACKEND:-http://127.0.0.1:5611}（/api、/healthz）"
echo "  若浏览器里之前点过 DEMO 角标，localStorage['harness.apiMode'] 会覆盖默认值，"
echo "  请点右上角角标切回 REAL（或控制台 removeItem 后刷新）。"
echo "  后端需另开终端：./run.sh --dev --port 5611   （Windows：run.cmd --dev --port 5611）"
echo "  退出：Ctrl+C"
echo

if [ -n "$DRY_RUN" ]; then
  say "dry-run：以下命令未真正执行"
  echo "  (cd web && HARNESS_BACKEND=\"\${HARNESS_BACKEND}\" npm run ${NPM_SCRIPT} -- ${VITE_ARGS[*]})"
  exit 0
fi

# ---------------------------------------------------------------------------
# 启动（exec 让 vite 成为前台进程，Ctrl+C 直接可达，不留孤儿进程）
# ---------------------------------------------------------------------------
cd web
exec npm run "$NPM_SCRIPT" -- "${VITE_ARGS[@]}"
