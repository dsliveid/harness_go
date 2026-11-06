#!/bin/sh
#
# ===========================================================================
# harness-go one-click launcher for Linux / macOS distributions (tar.gz).
#
# What it does:
#   1) runs ./harness-go with a data directory next to this script
#   2) waits until GET /healthz answers, then prints the URL (and opens a
#      browser when possible)
#
# Optional arguments are forwarded to the binary, for example:
#   ./start.sh -addr 0.0.0.0:5611       expose to the local network
#   ./start.sh -log-level debug         verbose logging
#   ./start.sh -config my.yaml          use a custom config file
#
# Stop: Ctrl+C (SIGINT is forwarded, the server shuts down gracefully).
# ===========================================================================

set -eu

# Always run relative to this script's directory
cd "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"

BIN="./harness-go"
if [ ! -x "$BIN" ]; then
  if [ -f "$BIN" ]; then
    chmod +x "$BIN" 2>/dev/null || true
  fi
fi
if [ ! -f "$BIN" ]; then
  echo "[错误] 未在当前目录找到 harness-go 可执行文件。" >&2
  echo "       请完整解压压缩包后，在解压目录内执行 ./start.sh" >&2
  exit 1
fi
if [ ! -x "$BIN" ]; then
  echo "[错误] $BIN 没有可执行权限，请执行：chmod +x ./harness-go" >&2
  exit 1
fi

mkdir -p data

# 解析监听地址（用于探测与打印访问 URL），与二进制默认值保持一致（127.0.0.1:5611）
ADDR="127.0.0.1:5611"
prev=""
for a in "$@"; do
  case "$prev" in
    -addr|--addr) ADDR="$a" ;;
  esac
  case "$a" in
    -addr=*|--addr=*) ADDR="${a#*=}" ;;
  esac
  prev="$a"
done

HOST="${ADDR%:*}"
PORT="${ADDR##*:}"
case "$HOST" in
  0.0.0.0|'::'|'[::]'|'') HOST="127.0.0.1" ;;
esac
BASE_URL="http://${HOST}:${PORT}"

echo "==> 启动 harness-go（数据目录：$(pwd)/data）"
echo "==> 访问地址：${BASE_URL}    停止：Ctrl+C"
if [ -f data/initial_password ]; then
  echo "==> 初始账号口令见：data/initial_password"
fi
echo

# 就绪探测（后台存在时才有意义；这里与前台进程并存）
wait_ready() {
  url="$1/healthz"

  # 无 curl/wget 时直接放弃探测：/bin/sh 多为 dash，不支持 /dev/tcp，
  # 硬写会报 "not found" 噪声，反而干扰用户判断（服务本身照常工作）。
  if command -v curl >/dev/null 2>&1; then
    probe() { curl -fsS --max-time 2 "$url" >/dev/null 2>&1; }
  elif command -v wget >/dev/null 2>&1; then
    probe() { wget -q -T 2 -O /dev/null "$url" >/dev/null 2>&1; }
  else
    return 1
  fi

  i=0
  while [ "$i" -lt 60 ]; do
    probe && return 0
    i=$((i + 1))
    sleep 0.5
  done
  return 1
}

open_browser() {
  case "$(uname -s 2>/dev/null || echo unknown)" in
    Darwin) open "$1" >/dev/null 2>&1 || true ;;
    *)      xdg-open "$1" >/dev/null 2>&1 || true ;;
  esac
}

# 前台运行；信号由 shell 直接传给二进制（不做后台+wait，保持最简）
( wait_ready "$BASE_URL" && { echo "==> 服务已就绪：${BASE_URL}"; open_browser "$BASE_URL"; } ) &
"$BIN" -data data "$@"
