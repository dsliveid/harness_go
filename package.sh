#!/usr/bin/env bash
#
# harness-go 打包脚本（多平台交叉编译 → 分发包 → 校验和）
# 用法：./package.sh [选项]        （Windows：package.cmd）
#
# 选项：
#   --targets LIST   目标平台，逗号分隔（默认 windows/amd64,linux/amd64,darwin/arm64）
#                    例：--targets "windows/amd64,linux/arm64"
#   --no-web         跳过前端构建（复用已有 web/dist；缺失则包内无 UI）
#   --web            强制重建前端产物
#   --debug          调试构建（不 strip）
#   --nsis           额外尝试生成 Windows NSIS 安装包（需 makensis；缺失仅告警）
#   -h, --help       显示本帮助
#
# 产物：dist/
#   harness-go-<ver>-<os>-<arch>/     解压即用的分发包目录
#   harness-go-<ver>-<os>-<arch>.zip  （windows）或 .tar.gz（linux/darwin）
#   SHA256SUMS.txt                    全部压缩包的校验和
#
# 设计说明：
#   - 纯 Go 依赖（modernc.org/sqlite 无 CGO），故 CGO_ENABLED=0 交叉编译可行；
#   - 分发包内含二进制 + config.example.yaml + start.cmd/start.sh + README.txt，
#     用户解压即用，无需 Go / Node 运行时；
#   - 包内不含 data/（首次启动自动创建），避免把个人数据带进发布包。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

# 公共库：say / warn / die / human_size / harness_versions / harness_ldflags / ensure_web
. "$ROOT/scripts/lib/common.sh"

# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------
TARGETS="windows/amd64,linux/amd64,darwin/arm64"
FORCE_WEB=""
NO_WEB=""
DEBUG=""
NSIS=""

while [ $# -gt 0 ]; do
  case "$1" in
    --targets)   TARGETS="${2:-}"; shift ;;
    --targets=*) TARGETS="${1#*=}" ;;
    --web)       FORCE_WEB=1 ;;
    --no-web)    NO_WEB=1 ;;
    --debug)     DEBUG=1 ;;
    --nsis)      NSIS=1 ;;
    -h|--help)
      sed -n '3,25p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "错误：未知参数 $1" >&2
      exit 2
      ;;
  esac
  shift
done

[ -n "$TARGETS" ] || { echo "错误：--targets 不能为空。" >&2; exit 2; }

# ---------------------------------------------------------------------------
# 校验和（跨平台，统一输出 sha256sum 兼容格式 "<hex>  <文件名>"）
#
# 必须做回退探测：本机实测 Git Bash 自带的 sha256sum / md5sum 会报
#   "failed to set file descriptor text/binary mode: Bad file descriptor"
# （MSYS 的 O_BINARY 设置问题，与文件无关），直接调用会让打包在最后一步失败。
# 依次尝试：sha256sum → shasum -a 256 → openssl dgst -sha256。
# ---------------------------------------------------------------------------
SHA_MODE=""

detect_sha_backend() {
  local probe
  probe="$(mktemp 2>/dev/null || echo "/tmp/harness-probe.$$")"
  printf 'probe' >"$probe"

  if command -v sha256sum >/dev/null 2>&1 && sha256sum "$probe" >/dev/null 2>&1; then
    SHA_MODE="sha256sum"
  elif command -v shasum >/dev/null 2>&1 && shasum -a 256 "$probe" >/dev/null 2>&1; then
    SHA_MODE="shasum"
  elif command -v openssl >/dev/null 2>&1 && openssl dgst -sha256 "$probe" >/dev/null 2>&1; then
    SHA_MODE="openssl"
  else
    SHA_MODE=""
  fi
  rm -f "$probe"
}

# 打印 "<64位hex>  <文件路径>"（与 sha256sum 输出格式一致，便于用户核对）
sha256_line() {
  local file="$1" hex=""
  case "$SHA_MODE" in
    sha256sum) hex="$(sha256sum "$file" | awk '{print $1}')" ;;
    shasum)    hex="$(shasum -a 256 "$file" | awk '{print $1}')" ;;
    openssl)   hex="$(openssl dgst -sha256 "$file" | awk '{print $NF}')" ;;
    *)         return 1 ;;
  esac
  [ -n "$hex" ] || return 1
  echo "${hex}  ${file}"
}

# ---------------------------------------------------------------------------
# 1. 环境检查
# ---------------------------------------------------------------------------
command -v go >/dev/null 2>&1 || die "未找到 go，请先安装 Go 工具链：https://go.dev/dl/"

detect_sha_backend
if [ -z "$SHA_MODE" ]; then
  warn "未找到可用的 SHA-256 工具（sha256sum / shasum / openssl），将跳过校验和生成。"
else
  say "校验和工具：$SHA_MODE"
fi

# ---------------------------------------------------------------------------
# 2. 前端产物（内嵌进二进制的 UI；唯一构建入口是 web/build.sh）
# ---------------------------------------------------------------------------
if [ -n "$FORCE_WEB" ]; then
  ensure_web force || die "前端构建失败，已中止（如要复用旧产物请加 --no-web）。"
elif [ -n "$NO_WEB" ]; then
  ensure_web skip
else
  # 按需：dist 缺失或前端源码更新时重建（源码比产物新同样要重建，否则包里是旧 UI）
  ensure_web || die "前端构建失败，已中止（如要复用旧产物请加 --no-web）。"
fi
if [ ! -f web/dist/index.html ]; then
  warn "web/dist/index.html 不存在，打包产物将不含前端 UI（/ 显示提示页）。"
fi

# ---------------------------------------------------------------------------
# 3. 版本信息 + LDFLAGS（与 build.sh 同口径，统一由 common.sh 生成）
# ---------------------------------------------------------------------------
harness_versions
harness_ldflags "$DEBUG"
VERSION="$HARNESS_VERSION"
LDFLAGS="$HARNESS_LDFLAGS"
BUILD_PROFILE="$HARNESS_PROFILE"

# ---------------------------------------------------------------------------
# 4. 逐目标交叉编译 + 组装分发包
# ---------------------------------------------------------------------------
DIST="dist"
mkdir -p "$DIST"
# 只清理本脚本自己的产物（harness-go-* 目录与压缩包、校验和），
# 不用 rm -rf dist —— 避免把用户手动放在 dist/ 里的其他东西一并删掉。
rm -rf "$DIST"/harness-go-* "$DIST"/SHA256SUMS.txt "$DIST"/installer.nsi

[ -f packaging/README.txt ] || die "缺少 packaging/README.txt（分发包说明模板）。"
[ -f config.example.yaml ]  || die "缺少 config.example.yaml（分发包配置模板）。"

say "开始打包（version: ${HARNESS_VERSION}, commit: ${HARNESS_COMMIT}, profile: ${BUILD_PROFILE}）"
say "目标平台：${TARGETS}"

IFS=',' read -r -a target_list <<<"$TARGETS"
made=0

for t in "${target_list[@]}"; do
  t="$(echo "$t" | tr -d '[:space:]')"
  [ -n "$t" ] || continue

  GOOS="${t%%/*}"
  GOARCH="${t##*/}"
  if [ -z "$GOOS" ] || [ -z "$GOARCH" ] || [ "$GOOS" = "$GOARCH" ]; then
    die "目标格式应为 <os>/<arch>，收到：$t（例：windows/amd64）"
  fi

  NAME="harness-go-${VERSION}-${GOOS}-${GOARCH}"
  PKGDIR="${DIST}/${NAME}"
  EXE="harness-go"
  case "$GOOS" in windows) EXE="harness-go.exe" ;; esac

  say "[${GOOS}/${GOARCH}] 编译 → ${PKGDIR}/${EXE}"
  mkdir -p "$PKGDIR"

  # CGO_ENABLED=0：项目依赖全是纯 Go（modernc.org/sqlite），交叉编译无需 C 工具链。
  # 刻意不用 pipefail 之外的包装：编译失败必须立刻中止，否则会产出空包。
  if ! CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
      go build -trimpath -ldflags "$LDFLAGS" -o "${PKGDIR}/${EXE}" ./cmd/harness-go; then
    die "[${GOOS}/${GOARCH}] 编译失败。"
  fi

  # 包内附带：配置示例 + 启动脚本 + 说明
  cp config.example.yaml "${PKGDIR}/config.example.yaml"
  case "$GOOS" in
    windows)
      cp packaging/start.cmd "${PKGDIR}/start.cmd"
      ;;
    *)
      cp packaging/start.sh "${PKGDIR}/start.sh"
      chmod +x "${PKGDIR}/start.sh"
      # 关键：在 Windows 上交叉编译出的 ELF/Mach-O 不带可执行位（实测 -rw-r--r--），
      # 若不打上 0755，用户解压后 ./harness-go 会报 Permission denied。
      # tar.gz 会保留下面的权限位，所以必须在这里显式设置。
      chmod +x "${PKGDIR}/${EXE}"
      ;;
  esac
  cp packaging/README.txt "${PKGDIR}/README.txt"

  # 生成包内校验文件，便于用户自查二进制是否损坏
  if [ -n "$SHA_MODE" ]; then
    ( cd "$PKGDIR" && sha256_line "$EXE" >"${EXE}.sha256" )
  fi

  # 压缩：Windows 用 zip（PowerShell Compress-Archive 最通用，Git Bash 不自带 zip），
  #       其余平台用 tar.gz（保留可执行位，这是 Linux/macOS 的必要条件）。
  ARCHIVE=""
  case "$GOOS" in
    windows)
      if command -v zip >/dev/null 2>&1; then
        ( cd "$DIST" && zip -qr "${NAME}.zip" "$NAME" )
        ARCHIVE="${DIST}/${NAME}.zip"
      else
        if command -v powershell >/dev/null 2>&1; then
          powershell -NoProfile -Command \
            "Compress-Archive -Path '${PKGDIR}' -DestinationPath '${DIST}/${NAME}.zip' -Force" \
            >/dev/null
          ARCHIVE="${DIST}/${NAME}.zip"
        else
          warn "[${GOOS}/${GOARCH}] 既无 zip 也无 powershell，跳过压缩（仅保留目录）。"
        fi
      fi
      ;;
    *)
      # 注意：本机工作区所在盘是 MSYS 的 noacl 挂载（`D: on /d type ntfs (noacl)`），
      # chmod 对目录内的文件**完全无效**（实测 chmod +x 后仍是 -rw-r--r--），
      # 所以不能依赖 chmod，必须用 tar 的 --mode 把权限位写进归档：
      #   harness-go / start.sh → 0755（缺了它用户解压后 ./harness-go 报 Permission denied）
      #   其余（配置/说明/校验和）→ 0644
      #
      # 必须分两段写：同一次 tar 调用里对同一批文件传多个 --mode 会让 tar
      # 把后出现的同名项当重复条目处理，产出 "hard link to ..." 的坏归档（实测）。
      # 先建 0755 的可执行文件，再逐个追加 0644 的其余文件。
      tmparch="$(mktemp)"
      tar --owner=0 --group=0 --mode="u=rwx,go=rx" -cf "$tmparch" -C "$DIST" \
        "$NAME/$EXE" "$NAME/start.sh"
      for f in "$PKGDIR"/*; do
        base="$(basename "$f")"
        [ "$base" = "$EXE" ] && continue
        [ "$base" = "start.sh" ] && continue
        tar --owner=0 --group=0 --mode="u=rw,go=r" -rf "$tmparch" -C "$DIST" "$NAME/$base"
      done
      gzip -c "$tmparch" >"${DIST}/${NAME}.tar.gz"
      rm -f "$tmparch"
      ARCHIVE="${DIST}/${NAME}.tar.gz"
      ;;
  esac

  if [ -n "$ARCHIVE" ] && [ -f "$ARCHIVE" ]; then
    size="$(ls -lh "$ARCHIVE" | awk '{print $5}')"
    echo "    → $(basename "$ARCHIVE")  ($size)"
  fi
  made=$((made + 1))
done

[ "$made" -gt 0 ] || die "没有任何目标被成功打包。"

# ---------------------------------------------------------------------------
# 5. 校验和清单
# ---------------------------------------------------------------------------
SUMS="${DIST}/SHA256SUMS.txt"
: >"$SUMS"
if [ -n "$SHA_MODE" ]; then
  ( cd "$DIST" && for f in *.zip *.tar.gz; do
      [ -f "$f" ] || continue
      sha256_line "$f" >>SHA256SUMS.txt
    done )
fi

# ---------------------------------------------------------------------------
# 6. 可选：Windows NSIS 安装包
# ---------------------------------------------------------------------------
if [ -n "$NSIS" ]; then
  if command -v makensis >/dev/null 2>&1; then
    WINDIR_BUILD=""
    for d in "$DIST"/harness-go-*-windows-amd64; do
      [ -d "$d" ] && WINDIR_BUILD="$d" && break
    done
    if [ -n "$WINDIR_BUILD" ]; then
      say "生成 NSIS 安装包（makensis）..."
      cat >"${DIST}/installer.nsi" <<NSI
!include "MUI2.nsh"
Name "harness-go ${VERSION}"
OutFile "${DIST}/harness-go-${VERSION}-windows-amd64-setup.exe"
InstallDir "\$PROFILE\\harness-go"
RequestExecutionLevel user
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"
Section "Install"
  SetOutPath "\$INSTDIR"
  File "${WINDIR_BUILD}/*.*"
  CreateShortCut "\$DESKTOP\\harness-go.lnk" "\$INSTDIR\\start.cmd"
SectionEnd
NSI
      if ( cd "$DIST" && makensis installer.nsi >/dev/null ); then
        echo "  安装包  ${DIST}/harness-go-${VERSION}-windows-amd64-setup.exe"
        if [ -n "$SHA_MODE" ]; then
          ( cd "$DIST" && sha256_line "harness-go-${VERSION}-windows-amd64-setup.exe" >>SHA256SUMS.txt )
        fi
      else
        warn "makensis 执行失败，已跳过安装包（源码包不受影响）。"
      fi
      rm -f "${DIST}/installer.nsi"
    else
      warn "--nsis 需要 windows/amd64 目标，本次未包含，已跳过。"
    fi
  else
    warn "未找到 makensis（NSIS），已跳过安装包生成。"
    warn "安装方式：https://nsis.sourceforge.io/Download（或 choco install nsis）"
  fi
fi

# ---------------------------------------------------------------------------
# 7. 汇总
# ---------------------------------------------------------------------------
echo
echo "==> 打包完成，产物位于 ${DIST}/："
for d in "$DIST"/*; do
  case "$d" in
    *.txt|*.zip|*.tar.gz) continue ;;
  esac
  [ -d "$d" ] || continue
  echo "  分发包目录  $d"
done
for f in "$DIST"/*.zip "$DIST"/*.tar.gz; do
  [ -f "$f" ] || continue
  echo "  压缩包      $f  ($(ls -lh "$f" | awk '{print $5}'))"
done
[ -s "$SUMS" ] && echo "  校验和      $SUMS"
echo
echo "  用户拿到压缩包后：解压 → Windows 运行 start.cmd / Linux·macOS 运行 ./start.sh"
echo "  单独编译调试请用：./build.sh        启动服务请用：./run.sh"
