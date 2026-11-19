# 缓存命中率与上下文治理体检（直查 SQLite，不依赖服务在线）
#
# # 为什么需要这个脚本（2026-11 教训）
#
# 事故会话 122559a3 烧到 839 万 token、耗时 15.9 分钟，而：
#   - session_compactions = 0（压缩从未发生）
#   - data/logs/logs-YYYYMMDD.db 的 llm_requests 全表 0 行（明细埋点为空）
# 也就是说「压缩到底有没有省到钱」当时**完全不可复盘**，只能靠人肉读 SQL 推断。
#
# 本脚本把复盘所需的最小事实一次查全，全部使用**已经落库的字段**：
#   - runs.cached_tokens / runs.input_tokens  → 单 Run 命中率（真值，provider 回传）
#   - messages.usage_json                     → 逐步 prompt/completion/cached（真值）
#   - messages 行数 / 空正文 assistant 数      → 「结果不显示」的结构性风险
#   - session_compactions                     → 压缩是否真的发生过
#
# 用法：
#   powershell -NoProfile -File scripts/audit-cache-hit.ps1
#   powershell -NoProfile -File scripts/audit-cache-hit.ps1 -SessionId 122559a3...
#
# 退出码：0 = 正常；1 = 发现高危项（长会话零压缩 / 首屏被截断风险）

param(
    [string]$DataDir = "data",
    [string]$SessionId = "",
    # 超过该条数即视为「前端首屏会被截断」的风险会话（服务端默认 limit=100）
    [int]$MessageWarnThreshold = 100,
    # 超过该 token 数却零压缩即视为高危（压缩水位 = 65536 * 80% ≈ 52k）
    [int]$TokenWarnThreshold = 52429
)

$ErrorActionPreference = "Stop"
$script:problems = 0

function Get-Sqlite {
    param([string]$Db, [string]$Query)
    $out = & sqlite3 $Db $Query 2>&1
    if ($LASTEXITCODE -ne 0) { throw "sqlite3 查询失败: $Query`n$out" }
    return $out
}

# Get-SqliteOptional 允许表不存在（老会话库可能缺少新表），返回 $null。
#
# 为什么需要：部分会话库是在 session_compactions 表引入之前创建的，
# 直接查会报「no such table」—— 那是**历史形态**而非缺陷，不应中断体检。
function Get-SqliteOptional {
    param([string]$Db, [string]$Query)
    # 2>$null 抑制 stderr：否则「no such table」这类提示会作为 ErrorRecord 打到控制台，
    # 让「可容忍的历史形态」看起来像脚本故障。
    $out = & sqlite3 $Db $Query 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    return $out
}

function Write-Section {
    param([string]$Title)
    Write-Host ""
    Write-Host "=== $Title" -ForegroundColor Cyan
}

function Write-Problem {
    param([string]$Msg)
    $script:problems++
    Write-Host "  [高危] $Msg" -ForegroundColor Red
}

function Write-OK {
    param([string]$Msg)
    Write-Host "  [正常] $Msg" -ForegroundColor Green
}

$mainDb = Join-Path $DataDir "main.db"
if (-not (Test-Path $mainDb)) { throw "找不到主库: $mainDb（请在仓库根目录运行，或用 -DataDir 指定）" }

Write-Section "主库会话概览（$mainDb）"
$sessions = Get-Sqlite $mainDb @"
SELECT s.id || '|' || COALESCE(NULLIF(s.title,''),'(无标题)') || '|' ||
       COALESCE(s.last_run_status,'') || '|' || COALESCE(s.total_tokens,0)
FROM sessions s WHERE s.deleted_at IS NULL ORDER BY s.updated_at DESC LIMIT 20;
"@
$sessionRows = @()
foreach ($line in ($sessions -split "`n")) {
    if (-not $line.Trim()) { continue }
    $p = $line -split '\|'
    if ($p.Count -lt 4) { continue }
    $sessionRows += [pscustomobject]@{ Id = $p[0]; Title = $p[1]; Status = $p[2]; TotalTokens = [int64]$p[3] }
}
if ($sessionRows.Count -eq 0) {
    Write-Host "  （无会话）"
} else {
    $sessionRows | Format-Table -AutoSize | Out-String | Write-Host
}

Write-Section "逐会话体检（消息数 / 空正文 / 压缩次数 / 命中率）"
foreach ($s in $sessionRows) {
    if ($SessionId -and $s.Id -ne $SessionId) { continue }
    $db = Join-Path $DataDir "sessions/$($s.Id)/session.db"
    if (-not (Test-Path $db)) {
        Write-Host "  - $($s.Id) [$($s.Title)] 无会话库（尚未落消息）"
        continue
    }

    $msgCount = [int](Get-Sqlite $db "SELECT COUNT(*) FROM messages;")
    $emptyAsst = [int](Get-Sqlite $db "SELECT COUNT(*) FROM messages WHERE role='assistant' AND LENGTH(TRIM(content))=0;")
    $asstCount = [int](Get-Sqlite $db "SELECT COUNT(*) FROM messages WHERE role='assistant';")
    $compactionsRaw = Get-SqliteOptional $db "SELECT COUNT(*) FROM session_compactions;"
    $compactions = if ($null -eq $compactionsRaw) { -1 } else { [int]$compactionsRaw }
    # 单 Run 真值：cached / input 是 provider 直接回传的两个数
    $runStat = Get-Sqlite $db @"
SELECT COALESCE(SUM(input_tokens),0) || '|' || COALESCE(SUM(cached_tokens),0)
FROM runs;
"@
    $rp = ($runStat -split '\|')
    $inTok = [int64]$rp[0]; $cacheTok = [int64]$rp[1]
    $rate = if ($inTok -gt 0) { [math]::Round($cacheTok * 100.0 / $inTok, 1) } else { 0 }
    $maxPrompt = Get-Sqlite $db @"
SELECT COALESCE(MAX(CAST(json_extract(usage_json,'`$.prompt_tokens') AS INTEGER)),0)
FROM messages WHERE usage_json IS NOT NULL;
"@
    $maxPrompt = [int64]$maxPrompt

    Write-Host ""
    Write-Host "  - $($s.Id) [$($s.Title)] status=$($s.Status)"
    $compLabel = if ($compactions -lt 0) { "（老库无该表）" } else { "$compactions 次" }
    Write-Host "      消息 $msgCount 条 / assistant $asstCount 条（其中空正文 $emptyAsst 条）/ 压缩 $compLabel"
    Write-Host "      token: input=$inTok cached=$cacheTok 命中率=$rate%  单请求峰值 prompt=$maxPrompt"

    if ($msgCount -gt $MessageWarnThreshold) {
        Write-Problem "消息 $msgCount 条 > 前端单页 $MessageWarnThreshold 条：首屏必须依赖 has_more + 向上分页，否则最终结论不可见"
    }
    if ($compactions -eq 0 -and $maxPrompt -ge $TokenWarnThreshold) {
        Write-Problem "单请求峰值 $maxPrompt tokens 已越压缩水位（$TokenWarnThreshold）却零压缩：压缩真空复发"
    }
    if ($asstCount -gt 0 -and $emptyAsst -eq $asstCount) {
        Write-Problem "全部 $asstCount 条 assistant 正文为空：交付轨必然空白（依赖前端兜底条）"
    }
    if ($inTok -gt 0 -and $rate -lt 30) {
        # 短会话（单请求 < 4k）本来就没多少可命中的前缀，不构成缺陷；
        # 命中率只在「长会话仍低」时才有诊断价值（正常应在 90% 以上）。
        if ($maxPrompt -ge 4000) {
            Write-Problem "缓存命中率仅 $rate%：检查 system 是否在单 Run 内变化（promote 污染前缀）"
        } else {
            Write-Host "      （短会话，命中率无诊断意义，忽略）" -ForegroundColor DarkGray
        }
    }
}

Write-Section "审计明细（天库）"
$logDir = Join-Path $DataDir "logs"
if (Test-Path $logDir) {
    $dbs = Get-ChildItem $logDir -Filter "logs-*.db" -ErrorAction SilentlyContinue
    if (-not $dbs) {
        Write-Host "  （无天库）"
    } else {
        foreach ($f in $dbs) {
            $c = & sqlite3 $f.FullName "SELECT COUNT(*) FROM llm_requests;" 2>&1
            Write-Host ("  {0}  llm_requests={1}" -f $f.Name, $c)
        }
        Write-Host ""
        Write-Host "  说明：llm_requests 为空说明审计埋点未产出（档位 off 或从未跑过 Run），" -ForegroundColor Yellow
        Write-Host "        这不影响上面的复盘结论 —— 命中率一律以 runs.cached_tokens 为准。" -ForegroundColor Yellow
    }
}

Write-Section "结论"
if ($script:problems -eq 0) {
    Write-OK "未发现缓存命中 / 上下文治理的高危项"
    exit 0
}
Write-Host "  共 $($script:problems) 项高危，见上方 [高危] 行。" -ForegroundColor Red
exit 1
