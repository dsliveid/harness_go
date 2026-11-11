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
# 前端产物完整性校验：web/dist/index.html 引用的 assets/* 是否都真实存在
#
# 为什么需要它：构建失败有两种截然不同的后果，必须区别对待。
#   ① 失败于 tsc -b（类型检查阶段）：vite 尚未写出任何文件，dist 仍是**上一次的
#      完整产物** → 可安全复用；
#   ② 失败于 vite build 中途：index.html 可能已被覆写、新 chunk 未写全 → dist
#      **半损坏**，此时若仍内嵌进二进制，页面必然白屏，且现象极难排查。
# 本函数用于区分这两种情况：返回 0 表示「dist 完整可用」。
#
# 实现：解析 index.html 里的 assets/xxx 引用，逐个 stat；一个都不能缺。
#   只用 grep（System32 无同名工具），与 web/build.sh 的 prune 逻辑口径一致。
# ---------------------------------------------------------------------------
dist_intact() {
  local html="web/dist/index.html"
  [ -f "$html" ] || return 1
  local refs
  refs="$(grep -o 'assets/[A-Za-z0-9._/-]*' "$html" 2>/dev/null | sort -u)"
  [ -n "$refs" ] || return 1          # 入口里一个 assets 引用都没有 → 视为异常
  local r
  for r in $refs; do
    [ -f "web/dist/$r" ] || return 1
  done
  return 0
}

# ---------------------------------------------------------------------------
# 前端产物新鲜度：以下白名单内的文件是否「比 web/dist/index.html 新」
#
# 实现约束（踩过的坑）：**禁止使用外部 find / sort / head**。
#   build.cmd / run.cmd 曾把 %SystemRoot%\System32 前置到 PATH，而 Windows 自带同名工具：
#     System32\find.exe 是「字符串查找」而非 GNU find → `-newer` 参数报错被吞、结果恒为空；
#     System32\sort.exe 同样不支持 -u。混用会让新鲜度判断静默失效。
#   故这里只用 bash 内建比较（[ a -nt b ]）与 glob，跨 Git Bash / MSYS 行为一致。
#
# ⚠️ 配置权限说明（2026-11）：
#   run.sh 已改为「启动时无条件重建前端」，正常路径不再依赖本判定；
#   本函数现在只服务于两类场景：① build.sh / package.sh 的按需构建；
#   ② run.sh --no-web-rebuild 时的兜底告警。因此白名单必须尽量收全，
#   一旦新增加固前述 5 类盲区，请在此同步补充，否则这两条路径会静默给出旧 UI。
# ---------------------------------------------------------------------------
web_stale() {
  [ -f web/dist/index.html ] || return 0   # 无产物 → 视为需要构建
  local f hit=""
  local prev_nullglob prev_globstar
  prev_nullglob="$(shopt -p nullglob 2>/dev/null || true)"
  prev_globstar="$(shopt -p globstar 2>/dev/null || true)"
  shopt -s nullglob globstar 2>/dev/null || true
  # ① 源码：不再只认 ts/tsx/css/json，把 vite 可直接处理的全部扩展名纳入，
  #    否则「把 svg/jsx 当组件或资源改一行」会被判定为新鲜，静默用旧 UI。
  # ② web/public/**：vite 原样拷贝进 dist 的静态资源（图标/robots/favicon 等）。
  # ③ 构建配置与依赖锁：package-lock.json 变了（升级依赖）同样必须重建。
  # ④ build.sh 自身：构建脚本逻辑变更（如新增 prune 步骤）也算产物需更新。
  for f in web/src/**/*.ts web/src/**/*.tsx web/src/**/*.js web/src/**/*.jsx \
           web/src/**/*.mjs web/src/**/*.cjs web/src/**/*.css web/src/**/*.scss \
           web/src/**/*.sass web/src/**/*.less web/src/**/*.json web/src/**/*.svg \
           web/src/**/*.html \
           web/public/** \
           web/index.html web/vite.config.ts web/vite.config.js \
           web/tailwind.config.js web/tailwind.config.ts \
           web/postcss.config.js web/postcss.config.cjs \
           web/package.json web/package-lock.json \
           web/tsconfig.json web/tsconfig.node.json web/tsconfig.app.json \
           web/build.sh web/build.cmd; do
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
