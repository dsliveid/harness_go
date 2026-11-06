#!/usr/bin/env bash
#
# harness-go 脚本公共库（被 run.sh / build.sh / package.sh / web/*.sh 引用，不单独执行）
#
# 用法（必须用 source，不能用 bash 直接跑）：
#   . "$(cd "$(dirname "${BASH_SOURCE[0]}")/../scripts/lib" && pwd)/common.sh"
#   或引用项目根已经算好时：. "$ROOT/scripts/lib/common.sh"
#
# 抽取原则：只放「被两个及以上脚本逐字复用」的逻辑，避免二次漂移。
#   版本信息 / LDFLAGS     —— 原 build.sh:71-89,167-174 与 package.sh:174-199 逐字重复
#   前端产物新鲜度 web_stale —— 原 run.sh:221-236
#   确保前端产物 ensure_web —— 原 run.sh:257-272 与 build.sh/package.sh 的三份 npm 逻辑
#   say / warn / die / human_size —— 各脚本各一份
#
# 注意：本文件不做逻辑判断，只定义函数与变量，禁止在这里 cd 或执行副作用。

# 重复 source 保护
if [ "${HARNESS_COMMON_SH_LOADED:-}" = "1" ]; then
  return 0 2>/dev/null || true
fi
HARNESS_COMMON_SH_LOADED=1

# ---------------------------------------------------------------------------
# 日志
# ---------------------------------------------------------------------------
say()  { echo "==> $*"; }
warn() { echo "[警告] $*" >&2; }
die()  { echo "[错误] $*" >&2; exit 1; }

# 可读文件大小；ls -lh 不可用时回退字节数
human_size() {
  local out
  out="$(ls -lh "$1" 2>/dev/null | awk '{print $5}')"
  if [ -n "$out" ]; then
    echo "$out"
  else
    wc -c <"$1" 2>/dev/null | tr -d ' '
  fi
}

# ---------------------------------------------------------------------------
# 版本信息（-ldflags 注入，启动日志会打印）
# 优先级：VERSION 环境变量 > git tag > 兜底 0.1.0
# 必须在一处定义，否则 build.sh / package.sh 的版本号会漂移。
# 输出：HARNESS_VERSION / HARNESS_COMMIT / HARNESS_BUILDAT / HARNESS_LDFLAGS
# ---------------------------------------------------------------------------
harness_versions() {
  local version="${VERSION:-}"
  if [ -z "$version" ]; then
    if command -v git >/dev/null 2>&1; then
      version="$(git describe --tags --abbrev=0 2>/dev/null || true)"
      version="${version#v}"
    fi
  fi
  [ -n "$version" ] || version="0.1.0"

  local commit="none"
  if command -v git >/dev/null 2>&1; then
    commit="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
    # 工作区有未提交改动时标注 -dirty，避免把「脏树构建」误当发布版本
    if [ "$commit" != "none" ] && [ -n "$(git status --porcelain 2>/dev/null || true)" ]; then
      commit="${commit}-dirty"
    fi
  fi

  HARNESS_VERSION="$version"
  HARNESS_COMMIT="$commit"
  HARNESS_BUILDAT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}

# 生成 LDFLAGS；$1=debug 时保留符号（不 strip）
# 输出：HARNESS_LDFLAGS / HARNESS_PROFILE
harness_ldflags() {
  local debug="${1:-}"
  [ -n "${HARNESS_VERSION:-}" ] || harness_versions
  HARNESS_LDFLAGS="-X main.version=${HARNESS_VERSION} -X main.commit=${HARNESS_COMMIT} -X main.buildTime=${HARNESS_BUILDAT}"
  if [ -z "$debug" ]; then
    # -s -w 去符号表与 DWARF，体积通常减半；-trimpath 去掉本机绝对路径（可复现构建）
    HARNESS_LDFLAGS="-s -w ${HARNESS_LDFLAGS}"
    HARNESS_PROFILE="release"
  else
    HARNESS_PROFILE="debug"
  fi
}

# 本机二进制路径（bin/harness-go[.exe]）；输出到 stdout
harness_bin_path() {
  case "$(go env GOOS 2>/dev/null || echo unknown)" in
    windows) echo "bin/harness-go.exe" ;;
    *)       echo "bin/harness-go" ;;
  esac
}

# ---------------------------------------------------------------------------
# 前端产物新鲜度：web/src 等是否比 web/dist/index.html 新
#
# 痛点：web/dist 通过 //go:embed 在**编译期**打进二进制，Go 侧完全不感知它的变化。
#       后端脚本（go run / 已编译二进制）不会因为前端源码改动而生效：
#       页面仍是上一次 npm run build 的产物，用户会误以为「前端没变化 / 代码没生效」。
#
# 实现约束（踩过的坑）：**禁止使用外部 find / sort / head**。
#   build.cmd / run.cmd 曾把 %SystemRoot%\System32 前置到 PATH，而 Windows 自带同名工具：
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

# ---------------------------------------------------------------------------
# 确保前端产物就绪（唯一入口：委托 web/build.sh）
#   参数：$1 = force（强制重建）| skip（不构建）| ""（按需：dist 缺失或源码更新）
#   返回：0 成功；非 0 表示构建失败（是否致命由调用方决定）
# ---------------------------------------------------------------------------
ensure_web() {
  local mode="${1:-}"
  shift || true   # 第 1 个参数是模式，不向下转发给 web/build.sh
  case "$mode" in
    skip)
      if [ -f web/dist/index.html ]; then
        say "复用已有前端产物（需要重建请加 --web）。"
      else
        warn "web/dist/index.html 不存在，二进制内不含前端；启动后 / 会显示「前端未构建」提示页。"
      fi
      return 0
      ;;
    force)
      say "强制重建前端产物（web/build.sh）..."
      ;;
    *)
      if [ -f web/dist/index.html ] && ! web_stale; then
        say "复用已有前端产物（需要重建请加 --web）。"
        return 0
      fi
      if [ ! -f web/dist/index.html ]; then
        say "未发现前端产物 web/dist/index.html，将先构建前端。"
      else
        say "检测到前端源码比产物 web/dist 更新，重建前端（跳过请加 --no-web-rebuild）。"
      fi
      ;;
  esac

  [ -f web/build.sh ] || { warn "缺少 web/build.sh，无法构建前端。"; return 1; }
  bash web/build.sh "$@"
}
