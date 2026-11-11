#!/usr/bin/env bash
#
# harness-go 后端启动脚本（只起后端；前端热更新请用 web/run.sh）
# 用法：./run.sh [选项] [-- 透传给二进制的参数...]   （Windows：run.cmd）
#
# 启动模式（二选一，默认 run）：
#   --run            单二进制模式：复用/重建 bin/harness-go 后启动（默认模式）
#   --dev            开发模式：用 go run ./cmd/harness-go（改 Go 代码后重启即生效，
#                    不产出二进制）。注意：go run 不是热重载，改完仍需 Ctrl+C 重跑。
#
# 选项：
#   --port N         监听端口（默认 5611）
#   --host HOST      显式绑定地址（不含端口；如 --host 127.0.0.1 仅本机、--host 192.168.1.5、--host ::）
#   --lan            显式声明对局域网开放（与默认行为相同，保留为兼容写法）
#   --data DIR       数据目录（等价于 -data DIR，默认 ./data）
#   --config FILE    配置文件（等价于 -config FILE，默认 data/config.yaml）
#   --open           服务就绪后自动打开浏览器（仅 run 模式）
#   --no-build       二进制缺失时也不自动构建，直接报错（仅 run 模式）
#   --rebuild        启动前强制重新编译二进制（仅 run 模式）
#   --dry-run        只打印将要执行的命令，不真正启动
#   --no-health      不等待 /healthz 就绪（仅 run 模式）
#   --no-web-rebuild 跳过「启动前强制重建前端」，直接复用现有 web/dist
#                    （想加快启动或本机缺 Node 环境时用；此时页面可能是旧 UI）
#   -h, --help       显示本帮助
#
# 监听口径（2026-11 起，用户明确要求）：
#   **默认绑 0.0.0.0，局域网开箱可访问，无需任何附加参数**；启动横幅会列出可访问地址。
#   要收回仅本机访问：--host 127.0.0.1（或透传 -addr 127.0.0.1:N）。
#   局域网开放 = 无 TLS 明文暴露，请仅在可信内网使用。
#
# 其余参数请放在 `--` 之后，会原样透传给二进制，例如：
#   ./run.sh --port 9000 -- -log-level debug -mode server
#
# 停止：Ctrl+C（脚本会把信号转发给子进程并等待其优雅退出）
#
# ⚠️ 关于前端（每次启动都会无条件重建一次，这是刻意设计）：
#   web/dist 通过 //go:embed 在**编译期**打进二进制，Go 侧完全不感知它的变化，
#   故「运行中的进程」永远不会因前端改动而更新，必须重启。
#   为保证「启动那一刻的页面就是当前源码的最新产物」，本脚本在启动前**一律**
#   执行 web/build.sh 重建 web/dist；run 模式下随即强制重编二进制（否则内嵌的
#   仍是旧产物）。跳过重建请显式加 --no-web-rebuild。
#   要真正的 HMR（改组件即时生效），请另开终端跑前端 dev server：
#     终端 A：./run.sh --dev --port 5611
#     终端 B：./web/run.sh                （vite，5612，proxy 转给 5611）
#   详见 web/run.sh --help。

set -uo pipefail

# 无论从哪里调用，都先切到脚本所在目录（= 项目根）
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

# 公共库：say / warn / die / web_stale / ensure_web / harness_bin_path
. "$ROOT/scripts/lib/common.sh"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
MODE="run"          # run | dev
                    # ⚠️ 历史缺陷回顾：这里曾误写为 MODE="dev"，且参数解析里没有任何
                    #    选项能把它改回 "run"，导致脚本 100% 走 go run 分支：
                    #    --rebuild / --no-build / --no-health / --open 与整段单二进制
                    #    启动逻辑（就绪探测、initial_password 提示）全部静默失效。
                    #    现改为默认 run，并提供 --run 显式指定。
PORT="5611"         # ⚠️ 必须与 web/vite.config.ts 的 proxy 默认 target 一致（5611），
                    #    否则 ./run.sh 与 ./web/run.sh 两个终端互相连不上。
                    #    5611 同时与 internal/config 的默认监听地址保持一致。
BIND_HOST=""        # 显式绑定 host（--host / --lan 设置）；空 = 默认 127.0.0.1
LAN_MODE=""         # --lan：绑 0.0.0.0 对局域网开放
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
    --run)      MODE="run" ;;
    --dev)      MODE="dev" ;;
    --port)     need_val "$@"; PORT="$2"; shift ;;
    --port=*)   PORT="${1#*=}" ;;
    --lan)      LAN_MODE=1 ;;
    --host)     need_val "$@"; BIND_HOST="$2"; shift ;;
    --host=*)   BIND_HOST="${1#*=}" ;;
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
    --web)
      # 旧版 run.sh 的 --web 直接起 vite；现已拆到 web/run.sh，这里给出明确指引
      echo "[错误] --web 已迁移到前端专属脚本，不再由本脚本处理。" >&2
      echo "       请用：./web/run.sh    （Windows：web\\run.cmd）" >&2
      echo "       本脚本现在只负责后端启动；前后端同时开发需开两个终端。" >&2
      exit 2
      ;;
    -h|--help)
      sed -n '3,33p' "$0" | sed 's/^# \{0,1\}//'
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

# ---------------------------------------------------------------------------
# 监听地址组装（优先级：透传 -addr > --host > --lan > 默认全网卡）
# ---------------------------------------------------------------------------
# 监听口径（2026-11 起，用户明确要求）：**默认 0.0.0.0:N，局域网开箱可访问**，
# 不再需要加 --lan（该选项保留仅作显式写法兼容）。历史口径曾是默认 127.0.0.1
# 仅本机（保守安全姿态），随产品决策翻转；要收回仅本机访问用 --host 127.0.0.1
# 或透传 -addr 127.0.0.1:N。无 TLS 明文暴露由「可信内网部署」的产品定位承接。
#
# 三种设置监听的路径互斥告警（都设置时按优先级生效，但必须让用户知道谁被忽略）
if [ -n "$ADDR" ] && [ -n "$BIND_HOST" ]; then
  warn "已同时透传 -addr=${ADDR} 与 --host=${BIND_HOST}：按 -addr 生效，--host 被忽略。"
fi
if [ -n "$ADDR" ] && [ -n "$LAN_MODE" ]; then
  warn "已同时透传 -addr=${ADDR} 与 --lan：按 -addr 生效，--lan 被忽略。"
fi
if [ -n "$BIND_HOST" ] && [ -n "$LAN_MODE" ]; then
  warn "已同时给 --host=${BIND_HOST} 与 --lan：按 --host 生效，--lan 被忽略。"
fi

if [ -z "$ADDR" ]; then
  if [ -n "$BIND_HOST" ]; then
    ADDR="${BIND_HOST}:${PORT}"
  else
    # --lan 与默认同值（0.0.0.0）：--lan 自 2026-11 起为显式兼容写法，不再是开关
    ADDR="0.0.0.0:${PORT}"
  fi
fi

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
  while [ "$i" -lt "$tries" ]; do
    if (exec 3<>"/dev/tcp/${host}/${URL_PORT}") 2>/dev/null; then
      exec 3>&- 2>/dev/null || true
      return 0
    fi
    i=$((i + 1))
    sleep 0.25
  done
  return 1
}

# ---------------------------------------------------------------------------
# 组装二进制启动参数
# ---------------------------------------------------------------------------
# 数据目录：--data 优先；否则显式传默认值，保证「从别处调用」时数据目录仍固定在项目内
[ -n "$DATA_DIR" ] || DATA_DIR="./data"

ARGS=("-data" "$DATA_DIR")
if [ -n "$CONFIG_FILE" ]; then
  ARGS+=("-config" "$CONFIG_FILE")
fi
# ⚠️ 2026-11 修复：曾在此硬编码 "127.0.0.1:${PORT}"，导致 --lan / --host 无论如何设置
#    都被这一行覆盖回仅本机监听（用户实测「run.sh 启动无法局域网访问」的直接原因）。
#    现改为使用上方已组装好的 ADDR（优先级：透传 -addr > --host > --lan > 默认回环）。
#    ⚠️ 透传里已带 -addr 时不得再追加（否则出现重复参数，flag 包虽以后者覆盖，
#    但冗余参数会污染启动日志、且与「-addr 透传 > --lan」的互斥告警口径不一致）。
PASSTHRU_HAS_ADDR=""
for a in "${PASSTHRU[@]}"; do
  case "$a" in
    -addr|--addr|-addr=*|--addr=*) PASSTHRU_HAS_ADDR=1 ;;
  esac
done
if [ -z "$PASSTHRU_HAS_ADDR" ]; then
  ARGS+=("-addr" "$ADDR")
fi
if [ ${#PASSTHRU[@]} -gt 0 ]; then
  ARGS+=("${PASSTHRU[@]}")
fi

# ---------------------------------------------------------------------------
# 前端产物：本脚本只做「委托 web/build.sh」，不再内联任何 npm 逻辑。
#
# 策略（2026-11 调整）：**启动时一律无条件重建前端**，不再做「源码是否比产物新」
#   的启发式判定。原因：embed 是编译期内嵌，页面是否最新 100% 取决于启动前
#   web/dist 的构建状态；而任何启发式（mtime 白名单比对）都只可能漏判 → 静默
#   给用户一个旧页面。宁可每次多花几秒，也要保证「启动那一刻 = 当前源码」。
#   --no-web-rebuild → skip（显式放弃新鲜度，用于缺 Node 环境或快速重启）
#
# 输出：WEB_REBUILT=1 表示本次确实执行了前端构建（调用方需据此重编二进制）。
#
# ⚠️ 构建失败的处理策略（分清「可安全降级」与「必须中止」两态）：
#   npm run build = tsc -b && vite build，失败点决定 dist 的状态：
#   ① tsc -b 类型检查失败 → vite 根本没跑，dist 仍是上一次的完整产物
#      → 用 dist_intact 校验完整性通过则**降级复用**并高声告警，保证服务仍可起；
#   ② vite build 写出中途失败 → dist 可能半损坏（index.html 引用不存在的 chunk）
#      → 必须中止，否则内嵌坏产物 = 白屏且极难排查。
#   另：dist 缺失/损坏且构建失败 → 中止（此时无任何可用产物）。
#   需要完全跳过重建，用 --no-web-rebuild（明确「我知道自己在用旧 dist」）。
# ---------------------------------------------------------------------------
WEB_REBUILT=""
ensure_web_for_run() {
  WEB_REBUILT=""
  if [ -n "$NO_WEB_REBUILD" ]; then
    warn "已按 --no-web-rebuild 跳过前端重建，将复用现有 web/dist（页面可能是旧 UI）。"
    ensure_web skip || true
    return 0
  fi
  say "启动前重建前端产物（保证页面为当前源码的最新构建；跳过请加 --no-web-rebuild）"
  if ensure_web force; then
    WEB_REBUILT=1
    return 0
  fi

  # ---- 构建失败：按 dist 完整性决定降级 or 中止 ----
  if dist_intact; then
    warn "前端构建失败，但现有 web/dist 经完整性校验可用（多半是 tsc 类型检查未过）。"
    warn "本次将复用旧产物启动 → 页面**不是**当前源码的最新版本。"
    warn "请修复前端编译错误后重启，或先确认改动是否仅为类型问题。"
    return 0
  fi
  die "前端构建失败，且 web/dist 缺失或已损坏，无法启动（否则页面必然白屏）。
       请修复构建错误后重试：cd web && npm run build
       若为 Node 内存不足：NODE_OPTIONS=--max-old-space-size=4096 ./run.sh
       若确认旧产物仍可用：./run.sh --no-web-rebuild"
}

# ---------------------------------------------------------------------------
# dev 模式：go run（改动即生效，但需要重启进程）
# ---------------------------------------------------------------------------
if [ "$MODE" = "dev" ]; then
  command -v go >/dev/null 2>&1 || die "未找到 go，请先安装：https://go.dev/dl/"
  say "开发模式启动（go run ./cmd/harness-go ${ARGS[*]}）"
  say "监听地址：${ADDR}    访问：${BASE_URL}"
  say "停止：Ctrl+C（改 Go 代码后需重启；前端热更新请用 ./web/run.sh）"
  if [ -n "$DRY_RUN" ]; then
    if [ -z "$NO_WEB_REBUILD" ]; then
      echo "    [dry-run] bash web/build.sh   # 启动前强制重建前端产物"
    else
      echo "    [dry-run] # 已按 --no-web-rebuild 跳过前端重建"
    fi
    echo "    [dry-run] go run ./cmd/harness-go ${ARGS[*]}"
    exit 0
  fi
  # 前端产物是编译期内嵌的，dev 模式不会自动跟随源码变化，必须显式重建一次
  ensure_web_for_run
  exec go run ./cmd/harness-go "${ARGS[@]}"
fi

# ---------------------------------------------------------------------------
# 默认模式：单二进制
#   二进制内嵌 frontdist，故「前端重建了」或「前端源码变了」都必须重编二进制，
#   否则启动后内嵌的仍是旧产物。这里不再只看 web_stale，而是：
#     ① 前端刚被重建过   → 构建（dist 全新，二进制必然内嵌旧产物）
#     ② 二进制缺失       → 构建
#     ③ --rebuild        → 构建
#     ④ 跳过前端重建且源码比 dist 新 → 构建（web_stale 兜底）
# ---------------------------------------------------------------------------
if [ -z "$DRY_RUN" ]; then
  # 先重建前端（或按 --no-web-rebuild 跳过），结果写入 WEB_REBUILT
  ensure_web_for_run
elif [ -z "$NO_WEB_REBUILD" ]; then
  # dry-run 不真跑构建，但语义上等价于「前端会重建 → 二进制必然重编」
  WEB_REBUILT=1
fi

BIN="$(harness_bin_path)"

build_needed=""
build_reason=""
if [ -n "$WEB_REBUILT" ]; then
  build_needed=1
  build_reason="前端产物刚重建，需重编二进制内嵌新产物"
elif [ -n "$REBUILD" ]; then
  build_needed=1
  build_reason="--rebuild 强制重建"
elif [ ! -f "$BIN" ]; then
  say "未找到二进制 $BIN。"
  if [ -n "$NO_BUILD" ]; then
    die "请先执行 ./build.sh 构建（或去掉 --no-build）。"
  fi
  build_needed=1
  build_reason="二进制不存在"
elif [ -n "$NO_WEB_REBUILD" ] && web_stale; then
  # 只有跳过前端重建时，才需要用 web_stale 判断「dist 本身已比源码旧」
  say "检测到前端源码比产物 web/dist 更新。"
  warn "已按 --no-web-rebuild 跳过前端重建，页面将显示旧版本 UI。"
  build_needed=1
  build_reason="前端源码比二进制内嵌的产物新"
fi

if [ -n "$build_needed" ]; then
  [ -f ./build.sh ] || die "缺少 build.sh，无法自动构建。"
  say "开始构建二进制（原因：${build_reason}）..."
  # --skip-web：前端已由 ensure_web_for_run 重建过，不必让 build.sh 再跑一遍 npm
  # dry-run 必须纯只读：连二进制都不编译，只在下方汇总区打印将要执行的命令
  if [ -z "$DRY_RUN" ]; then
    bash ./build.sh --skip-web || die "构建失败，已中止启动。"
  fi
fi

# ---------------------------------------------------------------------------
# 局域网访问提示（--lan / --host 开放时）：枚举本机非回环 IPv4 并打印可访问 URL
# ---------------------------------------------------------------------------
# 说明：Windows 下 Git Bash 没有 hostname -I，走 ipconfig 解析；
#       Linux 用 hostname -I；macOS 用 ifconfig。解析失败静默跳过（提示行不打印），
#       绝不因此阻断启动。
lan_urls() {
  local ips=()
  case "$(uname -s 2>/dev/null || echo unknown)" in
    MINGW*|MSYS*|CYGWIN*)
      # ipconfig 输出形如 "   IPv4 Address. . . . : 192.168.1.5"；中文系统为 "IPv4 地址"
      while IFS= read -r line; do
        local ip="${line##*: }"
        ip="${ip//[[:space:]]/}"
        [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && [[ "$ip" != 127.* ]] && ips+=("$ip")
      done < <(ipconfig 2>/dev/null | grep -i "IPv4")
      ;;
    Darwin)
      while IFS= read -r line; do
        local ip="${line##*inet }"; ip="${ip%%/*}"
        [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && [[ "$ip" != 127.* ]] && ips+=("$ip")
      done < <(ifconfig 2>/dev/null | grep "inet ")
      ;;
    *)
      # hostname -I：空格分隔的地址列表（含 IPv6），只取 IPv4 点分段
      for ip in $(hostname -I 2>/dev/null); do
        [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && [[ "$ip" != 127.* ]] && ips+=("$ip")
      done
      ;;
  esac
  for ip in "${ips[@]}"; do
    echo "http://${ip}:${URL_PORT}"
  done
}

echo
say "启动 harness-go（$BIN ${ARGS[*]}）"
say "监听地址：${ADDR}    访问：${BASE_URL}"
say "数据目录：${DATA_DIR}    停止：Ctrl+C"
# 监听可见性提示（URL_PORT 在上方已规整：0.0.0.0/:: 场景本机访问落回 127.0.0.1）。
# 默认（0.0.0.0）即局域网开放 → 打印可访问地址；显式收窄（127.0.0.1 等）→ 打印说明为何其它设备连不上。
case "$ADDR" in
  0.0.0.0:*|'::'*|'[::]':*)
    say "局域网开放：以下地址可从其它设备访问（明文 HTTP，请仅在可信内网使用）："
    lan_urls | while IFS= read -r u; do echo "    ${u}"; done
    ;;
  *)
    say "仅本机可访问：其它设备无法连接；局域网访问请改用 --host 0.0.0.0（或透传 -addr 0.0.0.0:${URL_PORT}）。"
    ;;
esac
echo

if [ -n "$DRY_RUN" ]; then
  if [ -z "$NO_WEB_REBUILD" ]; then
    echo "    [dry-run] bash web/build.sh   # 启动前强制重建前端产物"
  else
    echo "    [dry-run] # 已按 --no-web-rebuild 跳过前端重建"
  fi
  if [ -n "$build_needed" ]; then
    echo "    [dry-run] bash ./build.sh --skip-web   # ${build_reason}"
  fi
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
