#!/usr/bin/env bash
#
# harness-go 启动脚本
# 用法：./run.sh [选项] [-- 透传给二进制的参数...]   （Windows：run.cmd）
#
# 选项：
#   --dev            开发模式：后端用 go run ./cmd/harness-go（改 Go 代码重启即生效，不产出二进制）
#                    ⚠️ 前端**不会**热更新：web/dist 是编译期内嵌的，只有 npm run build 才更新。
#                    本脚本会在「前端源码比 dist 新」时自动重建一次（--no-web-rebuild 跳过）；
#                    要前端热更新请用 --web（vite dev server，接口经 proxy 转给后端）。
#   --web            起前端 vite dev server（5612，dev 态默认 mock 数据；Ctrl+C 退出）
#   --port N         监听端口（等价于 -addr 127.0.0.1:N，默认 8080）
#   --data DIR       数据目录（等价于 -data DIR，默认 ./data）
#   --config FILE    配置文件（等价于 -config FILE，默认 data/config.yaml）
#   --open           服务就绪后自动打开浏览器
#   --no-build       二进制缺失时也不自动构建，直接报错
#   --rebuild        启动前强制重新编译一次
#   --dry-run        只打印将要执行的命令，不真正启动
#   --no-health      不等待 /healthz 就绪（立即把控制权交回终端）
#   --no-web-rebuild 跳过「前端源码比产物新」时的自动重建（想用旧 UI 或自行构建时用）
#   -h, --help       显示本帮助
#
# 其余参数请放在 `--` 之后，会原样透传给二进制，例如：
#   ./run.sh --port 9000 -- -log-level debug -mode server
#
# 停止：Ctrl+C（脚本会把信号转发给子进程并等待其优雅退出）

set -uo pipefail

# 无论从哪里调用，都先切到脚本所在目录（= 项目根）
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
MODE="dev"          # run | dev | web
PORT="5612"
DATA_DIR=""
CONFIG_FILE=""
DO_OPEN=""
NO_BUILD=""
REBUILD=""
DRY_RUN=""
NO_HEALTH=""
NO_WEB_REBUILD=""
PASSTHRU=()

need_val() { [ $# -ge 2 ] || { echo "错误：$1 需要参数值。" >&2; exit 2; }; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dev)      MODE="dev" ;;
    --web)      MODE="web" ;;
    --port)     need_val "$@"; PORT="$2"; shift ;;
    --port=*)   PORT="${1#*=}" ;;
    --data)     need_val "$@"; DATA_DIR="$2"; shift ;;
    --data=*)   DATA_DIR="${1#*=}" ;;
    --config)   need_val "$@"; CONFIG_FILE="$2"; shift ;;
    --config=*) CONFIG_FILE="${1#*=}" ;;
    --open)     DO_OPEN=1 ;;
    --no-build) NO_BUILD=1 ;;
    --rebuild)  REBUILD=1 ;;
    --dry-run)  DRY_RUN=1 ;;
    --no-health) NO_HEALTH=1 ;;
    --no-web-rebuild) NO_WEB_REBUILD=1 ;;
    -h|--help)
      sed -n '3,25p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    --)
      shift
      PASSTHRU=("$@")
      break
      ;;
    *)
      echo "错误：未知参数 $1（如需透传给二进制，请用 -- 分隔）" >&2
      echo "      查看帮助：./run.sh --help" >&2
      exit 2
      ;;
  esac
  shift
done

say()  { echo "==> $*"; }
warn() { echo "[警告] $*" >&2; }
die()  { echo "[错误] $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 端口规整
# ---------------------------------------------------------------------------
if [ -n "$PORT" ]; then
  case "$PORT" in
    ''|*[!0-9]*) die "--port 必须是数字，收到：$PORT" ;;
  esac
  if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    die "--port 必须在 1~65535 之间，收到：$PORT"
  fi
fi

# 从透传参数里也识别 -addr，便于 --open 拼出正确 URL
ADDR=""
for ((i = 0; i < ${#PASSTHRU[@]}; i++)); do
  case "${PASSTHRU[$i]}" in
    -addr|--addr) ADDR="${PASSTHRU[$((i + 1))]:-}" ;;
    -addr=*|--addr=*) ADDR="${PASSTHRU[$i]#*=}" ;;
  esac
done

if [ -n "$PORT" ]; then
  ADDR="127.0.0.1:${PORT}"
fi
[ -n "$ADDR" ] || ADDR="127.0.0.1:8080"

# 监听 "0.0.0.0" / "::" 时本机访问用 127.0.0.1
URL_HOST="${ADDR%:*}"
URL_PORT="${ADDR##*:}"
case "$URL_HOST" in
  0.0.0.0|'::'|'[::]'|'') URL_HOST="127.0.0.1" ;;
esac
BASE_URL="http://${URL_HOST}:${URL_PORT}"

# ---------------------------------------------------------------------------
# 浏览器打开（三平台）
# ---------------------------------------------------------------------------
open_browser() {
  local url="$1"
  case "$(uname -s 2>/dev/null || echo unknown)" in
    MINGW*|MSYS*|CYGWIN*)
      cmd //c start "" "$url" >/dev/null 2>&1 || warn "自动打开浏览器失败，请手动访问 $url"
      ;;
    Darwin)
      open "$url" >/dev/null 2>&1 || warn "自动打开浏览器失败，请手动访问 $url"
      ;;
    *)
      xdg-open "$url" >/dev/null 2>&1 || warn "自动打开浏览器失败，请手动访问 $url"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# 健康探测：GET /healthz 返回 200 即就绪
#   铁律：用 /healthz，不是 /api/v1/health（后者不存在）。
# ---------------------------------------------------------------------------
wait_ready() {
  local url="$1/healthz" tries="${2:-40}" i=0
  if command -v curl >/dev/null 2>&1; then
    while [ "$i" -lt "$tries" ]; do
      if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then return 0; fi
      i=$((i + 1))
      sleep 0.25
    done
    return 1
  fi
  # 无 curl 时退化为 TCP 探活（只能证明端口在听，不能证明 HTTP 正常）
  local host="${url#*//}"; host="${host%%/*}"; host="${host%:*}"
  local port="${url##*:}"
  i=0
  while [ "$i" -lt "$tries" ]; do
    if (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; then exec 3>&- 2>/dev/null; return 0; fi
    i=$((i + 1))
    sleep 0.25
  done
  return 1
}

# ---------------------------------------------------------------------------
# --web：只起前端 vite dev server（前端源码热更新，接口经 dev proxy 转给后端）
#
# 注意：本模式**只启动前端**，后端需另行启动（默认跑在 8080，可用
#       HARNESS_BACKEND=http://127.0.0.1:PORT 指定，vite.config.ts 会据此代理 /api 与 /healthz）。
# ---------------------------------------------------------------------------
if [ "$MODE" = "web" ]; then
  command -v node >/dev/null 2>&1 || die "未找到 node，请先安装：https://nodejs.org"
  command -v npm  >/dev/null 2>&1 || die "未找到 npm。"
  [ -d web/node_modules ] || { say "安装前端依赖（npm install）..."; ( cd web && npm install ); }

  say "启动前端开发服务器（vite，默认 http://127.0.0.1:5612，支持热更新）..."
  say "接口会经 dev proxy 转发到后端 ${HARNESS_BACKEND:-http://127.0.0.1:8080}（可用 HARNESS_BACKEND 覆盖）。"
  say "提示：dev 态默认 mock 数据；要连真实后端，浏览器控制台执行"
  say "      localStorage.setItem('harness.apiMode','real') 后刷新。"
  if [ -n "$DRY_RUN" ]; then
    echo "    [dry-run] (cd web && npm run dev)"
    exit 0
  fi
  ( cd web && exec npm run dev )
  exit $?
fi

# ---------------------------------------------------------------------------
# 组装二进制启动参数
# ---------------------------------------------------------------------------
# 数据目录：--data 优先；否则显式传默认值，保证「从别处调用」时数据目录仍固定在项目内
[ -n "$DATA_DIR" ] || DATA_DIR="./data"

ARGS=("-data" "$DATA_DIR")
if [ -n "$CONFIG_FILE" ]; then
  ARGS+=("-config" "$CONFIG_FILE")
fi
if [ -n "$PORT" ]; then
  ARGS+=("-addr" "127.0.0.1:${PORT}")
fi
if [ ${#PASSTHRU[@]} -gt 0 ]; then
  ARGS+=("${PASSTHRU[@]}")
fi

# ---------------------------------------------------------------------------
# 前端产物（web/dist）新鲜度保障
#
# 痛点：web/dist 通过 //go:embed 在**编译期**打进二进制，Go 侧完全不感知它的变化。
#       因此 `--dev`（go run）虽然后端改代码即生效，前端改了源码却**不会**生效：
#       页面仍是上一次 npm run build 的产物，用户会误以为「前端没变化 / 代码没生效」。
# 对策：只要 web/src 或 web/index.html 里有文件比 web/dist/index.html 新，
#       就自动重建一次前端（可加 --no-web-rebuild 跳过）。
#
# 实现约束（踩过的坑）：**禁止使用外部 find / sort / head**。
#   run.cmd 会把 %SystemRoot%\System32 前置到 PATH，而 Windows 自带同名工具：
#     System32\find.exe 是「字符串查找」而非 GNU find → `-newer` 参数报错被吞、结果恒为空；
#     System32\sort.exe 同样不支持 -u。混用会让新鲜度判断静默失效。
#   故这里只用 bash 内建比较（[ a -nt b ]）与 glob，跨 Git Bash / MSYS 行为一致。
# ---------------------------------------------------------------------------
web_stale() {
  [ -f web/dist/index.html ] || return 0   # 无产物 → 视为需要构建
  local f hit=""
  local prev_nullglob prev_globstar
  prev_nullglob="$(shopt -p nullglob 2>/dev/null || true)"
  prev_globstar="$(shopt -p globstar 2>/dev/null || true)"
  shopt -s nullglob globstar 2>/dev/null || true
  for f in web/src/**/*.ts web/src/**/*.tsx web/src/**/*.css web/src/**/*.json \
           web/index.html web/vite.config.ts web/tailwind.config.js \
           web/postcss.config.js web/package.json web/tsconfig.json; do
    if [ "$f" -nt web/dist/index.html ]; then hit="$f"; break; fi
  done
  eval "${prev_nullglob:-shopt -u nullglob}" 2>/dev/null || true
  eval "${prev_globstar:-shopt -u globstar}" 2>/dev/null || true
  [ -n "$hit" ]
}

# 清理 index.html 不再引用的历史产物。原因：vite 配了 emptyOutDir:false（为保住 .gitkeep），
# 旧 index-<hash>.js 不会自动删除，会被 //go:embed all:dist 全量打进二进制。
prune_assets() {
  local html="web/dist/index.html" f base content removed=0
  [ -f "$html" ] && [ -d web/dist/assets ] || return 0
  content="$(cat "$html")"
  for f in web/dist/assets/*; do
    [ -f "$f" ] || continue
    base="${f##*/}"
    case "$base" in .*) continue ;; esac
    case "$content" in
      *"$base"*) ;;
      *) rm -f "$f"; removed=$((removed + 1)) ;;
    esac
  done
  [ "$removed" -gt 0 ] && say "已清理 $removed 个历史前端产物（避免被 embed 打进二进制）。"
  return 0
}

ensure_web_fresh() {
  [ -n "$NO_WEB_REBUILD" ] && return 0
  command -v node >/dev/null 2>&1 || {
    warn "未找到 node，跳过前端产物重建（页面将显示旧版本 UI）。"
    return 0
  }
  web_stale || return 0
  say "检测到前端源码比产物 web/dist 更新，重建前端（跳过请加 --no-web-rebuild）..."
  [ -d web/node_modules ] || { say "安装前端依赖（npm install）..."; ( cd web && npm install ) || die "前端依赖安装失败。"; }
  if ( cd web && npm run build ); then
    prune_assets
    say "前端产物已更新。"
  else
    warn "前端构建失败，继续使用旧产物启动（页面可能是旧版本 UI）。"
  fi
}

# ---------------------------------------------------------------------------
# dev 模式：go run（改动即生效）
# ---------------------------------------------------------------------------
if [ "$MODE" = "dev" ]; then
  command -v go >/dev/null 2>&1 || die "未找到 go，请先安装：https://go.dev/dl/"
  say "开发模式启动（go run ./cmd/harness-go ${ARGS[*]}）"
  say "停止：Ctrl+C"
  if [ -n "$DRY_RUN" ]; then
    echo "    [dry-run] go run ./cmd/harness-go ${ARGS[*]}"
    if web_stale && [ -z "$NO_WEB_REBUILD" ]; then
      echo "    [dry-run] (cd web && npm run build)   # 前端源码比 web/dist 新，启动前会重建"
    fi
    exit 0
  fi
  # 前端产物是编译期内嵌的，dev 模式不会自动跟随源码变化，必须显式重建一次
  ensure_web_fresh
  go run ./cmd/harness-go "${ARGS[@]}"
  exit $?
fi

# ---------------------------------------------------------------------------
# 默认模式：单二进制
# ---------------------------------------------------------------------------
BIN="bin/harness-go"
case "$(go env GOOS 2>/dev/null || echo unknown)" in
  windows) BIN="bin/harness-go.exe" ;;
esac

build_needed=""
if [ -n "$REBUILD" ]; then
  build_needed=1
elif [ ! -f "$BIN" ]; then
  say "未找到二进制 $BIN。"
  if [ -n "$NO_BUILD" ]; then
    die "请先执行 ./build.sh 构建（或去掉 --no-build）。"
  fi
  build_needed=1
elif web_stale; then
  # 二进制存在，但前端源码比产物新：dist 内嵌进二进制，不重建就永远是旧 UI
  say "检测到前端源码比产物 web/dist 更新。"
  if [ -n "$NO_WEB_REBUILD" ]; then
    warn "已按 --no-web-rebuild 跳过前端重建，页面将显示旧版本 UI。"
  else
    say "需要重新构建二进制才能让新前端生效。"
    build_needed=1
  fi
fi

if [ -n "$build_needed" ]; then
  [ -x ./build.sh ] || die "缺少 build.sh，无法自动构建。"
  say "开始构建（前端有更新时 build.sh 会自动重建 web/dist）..."
  BUILD_ARGS=()
  if web_stale; then BUILD_ARGS+=(--web); fi
  ./build.sh "${BUILD_ARGS[@]}" || die "构建失败，已中止启动。"
fi

echo
say "启动 harness-go（$BIN ${ARGS[*]}）"
say "监听地址：${ADDR}    访问：${BASE_URL}"
say "数据目录：${DATA_DIR}    停止：Ctrl+C"
echo

if [ -n "$DRY_RUN" ]; then
  echo "    [dry-run] $BIN ${ARGS[*]}"
  exit 0
fi

# 后台启动 + 就绪探测 + 信号转发：
# 之所以不直接 exec，是因为需要在就绪后打开浏览器；前台语义由 wait 保持。
"$BIN" "${ARGS[@]}" &
CHILD_PID=$!

on_signal() {
  # 只发一次信号，避免用户连按 Ctrl+C 打断优雅关闭（服务端 Shutdown 30s）
  trap '' INT TERM
  if kill -0 "$CHILD_PID" 2>/dev/null; then
    kill -TERM "$CHILD_PID" 2>/dev/null || true
  fi
}
trap on_signal INT TERM

if [ -z "$NO_HEALTH" ]; then
  if wait_ready "$BASE_URL" 60; then
    say "服务已就绪：${BASE_URL}"
    # 首启会生成随机口令，提示用户去哪里拿
    if [ -f "${DATA_DIR}/initial_password" ]; then
      say "首启已生成初始账号，口令见：${DATA_DIR}/initial_password"
    fi
    [ -n "$DO_OPEN" ] && open_browser "$BASE_URL"
  else
    warn "15 秒内未探测到 ${BASE_URL}/healthz 就绪，继续等待（请查看上方日志排查）。"
  fi
elif [ -n "$DO_OPEN" ]; then
  open_browser "$BASE_URL"
fi

wait "$CHILD_PID"
RC=$?
trap - INT TERM
exit "$RC"
