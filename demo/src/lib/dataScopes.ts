import type { DataScope, DataScopeMode } from '../types'

// 数据权限：与工具审批相互独立。工具审批管「能不能执行这个动作」，
// 数据权限管「这次动作能触达哪些数据」，越界一律进入审批闸门。

export interface DataScopeMeta {
  key: DataScope
  label: string
  hint: string
  example: string
}

export const DATA_SCOPES: DataScopeMeta[] = [
  {
    key: 'fs_outside',
    label: '工作区外路径',
    hint: '读取项目工作区目录之外的文件',
    example: '如 ../harness_mini/src/** 或 F:/WorkSpace/data-pipeline/**',
  },
  {
    key: 'session_export',
    label: '会话数据导出',
    hint: '把会话消息、工具结果与制品导出到本地文件',
    example: '如 session-bundle.zip、制品图片下载',
  },
  {
    key: 'db_query',
    label: '数据库查询',
    hint: '通过 MCP / 插件查询数据库（只读 SELECT 亦需授权）',
    example: '如 postgres / sqlite MCP 的 query 工具',
  },
  {
    key: 'net_egress',
    label: '外部数据外发',
    hint: '把工作区内容发送到外部服务（MCP / HTTP 插件）',
    example: '如第三方 API、云端模型上传的附件',
  },
]

export const DATA_SCOPE_LABEL: Record<DataScope, string> = DATA_SCOPES.reduce(
  (acc, s) => ({ ...acc, [s.key]: s.label }),
  {} as Record<DataScope, string>,
)

export const DATA_SCOPE_HINT: Record<DataScope, string> = DATA_SCOPES.reduce(
  (acc, s) => ({ ...acc, [s.key]: s.hint }),
  {} as Record<DataScope, string>,
)

export const DATA_MODE_META: Array<{
  key: DataScopeMode
  label: string
  desc: string
}> = [
  { key: 'deny', label: '拒绝', desc: '直接拒绝并回传模型，不弹审批' },
  { key: 'ask', label: '询问', desc: '每次触达都进入审批闸门（推荐）' },
  { key: 'allow', label: '允许', desc: '自动放行，仅记录审计日志' },
]

export const DATA_MODE_LABEL: Record<DataScopeMode, string> = {
  deny: '拒绝',
  ask: '询问',
  allow: '允许',
}
