#!/usr/bin/env bash
# 重新生成设备协议黄金 fixture（供 hd-protocol 的漂移门禁使用）。
#
# 用法：
#   ./scripts/gen-protocol-fixtures.sh
#
# 何时必须运行：
#   - 改动 harness_go/internal/device/protocol.go 的任何结构体/字段；
#   - 改动 internal/model/device.go 的 Capability / 协议版本常量；
#   - 改动 internal/apperr 的 fs.* / device.* 错误码。
#
# 🔴 CI 会把「生成后 git diff 非空」判为失败 —— 说明有人改了协议但没提交 fixture，
#    下游 Rust 侧就会静默不同步（这是双写漂移的主要来源）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/../harness_desktop/crates/hd-protocol/fixtures"

if [[ ! -d "$(dirname "$OUT")" ]]; then
  echo "❌ 未找到目标目录，请确认 harness_desktop 与本仓同级：$OUT" >&2
  exit 1
fi

cd "$ROOT"
go run scripts/gen-fixtures.go -out "$OUT"
echo "✅ 已同步到 $OUT"
