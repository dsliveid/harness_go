/**
 * Agent 工具静态目录（文档性元数据，非演示业务数据）。
 *
 * 历史沿革（2026-11 正式版）：本文件内容原位于 `web/src/mock/settingsData.ts`，
 * 与 tokenStats / logEntries / agentSops / fallbackModelCatalog / fetchProviderModels
 * 等演示数据混放。正式版下线 mock 通路后，真正有价值的「工具说明元数据」
 * （标题 / 风险等级 / 可安装性 / 平台支持）被原样迁移至此，作为前端文档性常量保留；
 * 演示业务数据则随 mock 目录一并删除。
 *
 * 契约铁律：本文件只允许存放静态说明元数据，**严禁**写入任何伪造的业务数据
 * （工具执行结果、统计数字、模型清单等一律来自服务端真实接口）。
 */

import type { ToolGroup, ToolInfo, ToolInstallMeta } from '../types'

// ---------------- Agent 工具清单（对齐桌面端：标题 + 风险 + 类别） ----------------
export const toolList: ToolInfo[] = [
  // ---------- 文件与检索 ----------
  { group: 'file', name: 'read_file', title: '读取文件', risk: 'readonly', category: 'builtin', description: '读取工作区内文本文件的局部或全部内容，支持按行偏移切片。' },
  { group: 'file', name: 'list_dir', title: '列出目录', risk: 'readonly', category: 'builtin', description: '列出目录下的文件与子目录（含大小）。' },
  { group: 'file', name: 'file_outline', title: '提取大纲', risk: 'readonly', category: 'builtin', description: '提取代码文件的结构骨架（类/函数签名与行号），低成本掌握全貌。' },
  { group: 'file', name: 'glob', title: '查找文件', risk: 'readonly', category: 'builtin', description: '按 glob 模式（支持 ** 与 *）查找文件，返回相对路径列表。' },
  { group: 'file', name: 'grep', title: '搜索内容', risk: 'readonly', category: 'builtin', description: '按正则在工作区文件内容中搜索，支持上下文行展开。' },
  { group: 'file', name: 'write_file', title: '写入文件', risk: 'write', category: 'builtin', description: '新建文件或整体覆盖写入完整内容；仅用于新文件。' },
  { group: 'file', name: 'edit_file', title: '编辑文件', risk: 'write', category: 'builtin', description: '基于精确原文的最小化替换修改文件。' },
  { group: 'file', name: 'run_command', title: '执行命令', risk: 'danger', category: 'builtin', description: '在工作区目录执行 shell 命令（构建、测试、git 等），禁止破坏性命令。' },
  // ---------- 网络与图片 ----------
  { group: 'net', name: 'web_fetch', title: '网页抓取', risk: 'readonly', category: 'builtin', description: '通过 HTTP/HTTPS 抓取网页并清洗为纯文本（私有网段默认拦截，正文上限 32KB）。' },
  { group: 'net', name: 'save_image', title: '图片落盘', risk: 'write', category: 'builtin', description: '把 base64 图片数据写入工作区（PNG/JPEG/GIF/WebP 魔数校验，非法即拒）。' },
  { group: 'net', name: 'read_image_meta', title: '图片元信息', risk: 'readonly', category: 'builtin', description: '读取工作区图片的格式 / 宽高 / 大小，不经像素识别。' },
  // ---------- 任务与计划 ----------
  { group: 'plan', name: 'todo_write', title: '更新任务清单', risk: 'write', category: 'builtin', description: '整体覆写任务清单（每项含内容与状态 pending|in_progress|done）。' },
  { group: 'plan', name: 'todo_read', title: '读取任务清单', risk: 'readonly', category: 'builtin', description: '读取当前任务清单。' },
  { group: 'plan', name: 'plan_set', title: '设置执行计划', risk: 'write', category: 'builtin', description: '设置 / 更新执行计划（标题 + 步骤列表，步骤可标记完成）。' },
  { group: 'plan', name: 'plan_get', title: '读取执行计划', risk: 'readonly', category: 'builtin', description: '读取当前执行计划。' },
  // ---------- 长期记忆 ----------
  { group: 'memory', name: 'memory_write', title: '记录记忆', risk: 'write', category: 'builtin', description: '写入一条记忆（core / notes / archive 三层，启动时同步进检索索引）。' },
  { group: 'memory', name: 'memory_list', title: '列出记忆', risk: 'readonly', category: 'builtin', description: '列出工作区 .harness/memory/ 下已沉淀的记忆文件。' },
  // ---------- 子 Agent 与协作者 ----------
  { group: 'agent', name: 'spawn_subagent', title: '派生子 Agent', risk: 'write', category: 'builtin', description: '派生子代理执行独立子任务并阻塞等待结果（深度上限 2，禁递归；同会话在途上限 4）。' },
  { group: 'agent', name: 'ask_collaborator', title: '征询协作者', risk: 'write', category: 'builtin', description: '向指定常驻角色发起单次征询（无批量派发 / 等待 / 列举能力）。' },
  // ---------- 决策记录 ----------
  { group: 'decision', name: 'decision_record', title: '记录决策', risk: 'write', category: 'builtin', description: '记录一条关键决策。' },
  { group: 'decision', name: 'decision_list', title: '决策历史', risk: 'readonly', category: 'builtin', description: '列出历史决策（最近在前，默认 20 条）。' },
  { group: 'skill', name: 'skill_list', title: '查询技能库', risk: 'readonly', category: 'skill', description: '列出全部技能（名称 + 描述首行）。' },
  { group: 'skill', name: 'skill_read', title: '读取技能', risk: 'readonly', category: 'skill', description: '读取技能全文（.harness/skills/<name>/SKILL.md 正文）。' },
  { group: 'skill', name: 'skill_save', title: '沉淀技能', risk: 'write', category: 'skill', description: '沉淀 / 更新技能（写 .harness/skills/<name>/SKILL.md：标题行 + 正文步骤）。' },
]

// ---------------- 系统内置工具的可安装性（供「插件市场 → 工具」展示与客户端自装） ----------------
// 关键约定：内置工具必须「可安装」，且必须区分平台支持 —— 移动端 / PC 端（Windows、Linux 分开）。
// 服务端（Web 端）只是其中一端：客户端自行把能力装到自己所在的机器上执行；
// 纯服务端能力（如子 Agent / 子进程编排）标注不可安装，客户端仅通过 API 代理调用。
export const toolInstallMeta: Record<ToolGroup, ToolInstallMeta> = {
  file: { installable: true, mode: '随包内置', deps: ['仅标准库'], note: '纯本地文件系统操作，随客户端包体发布，无需额外下载。', platforms: { windows: true, linux: true, mobile: true } },
  net: { installable: true, mode: '按需下发', deps: ['network'], note: '网页抓取随包内置；图片工具只做落盘校验与元信息读取，不提供生成 / 像素识别（需图像模型槽位，provider 面暂无 capability 路由）。', platforms: { windows: true, linux: true, mobile: true } },
  plan: { installable: true, mode: '随包内置', deps: ['fs_write'], note: '任务清单与执行计划状态落服务端 .harness/，各端共用同一状态源。', platforms: { windows: true, linux: true, mobile: true } },
  memory: { installable: true, mode: '随包内置', deps: ['fs_write'], note: '记忆落本地 .harness/memory/，多端通过同步协议合并，避免双写冲突。', platforms: { windows: true, linux: true, mobile: true } },
  agent: { installable: false, mode: '不可安装', deps: [], note: '子 Agent / 协作者依赖服务端调度与并发信号量，任何客户端都不安装，仅通过 API 代理调用。', platforms: { windows: false, linux: false, mobile: false } },
  decision: { installable: true, mode: '随包内置', deps: ['fs_write'], note: '决策记录与历史列举是本地落盘能力，不依赖决策模型槽位。', platforms: { windows: true, linux: true, mobile: true } },
  skill: { installable: true, mode: '按需下发', deps: ['fs_read', 'fs_write'], note: '技能三件套随包内置；具体技能按需安装到客户端 .harness/skills/。', platforms: { windows: true, linux: true, mobile: true } },
}

/** 单工具覆盖（缺省沿用分组元信息）：平台差异集中在此声明，便于逐工具校准 */
export const toolInstallOverrides: Record<string, {
  mode?: ToolInstallMeta['mode']
  deps?: string[]
  platforms?: ToolInstallMeta['platforms']
  note?: string
}> = {
  // 命令执行：PC 端依赖 shell（Windows PowerShell / Linux bash），移动端沙箱不提供
  run_command: {
    mode: '随包内置',
    deps: ['shell'],
    platforms: { windows: true, linux: true, mobile: false },
    note: 'Windows 走 PowerShell、Linux 走 bash；移动端不提供 shell 执行能力。',
  },
  // 写入类工具：PC 端全支持，移动端仅提供受限沙箱写入
  write_file: { platforms: { windows: true, linux: true, mobile: true }, note: '移动端写入受沙箱目录限制（仅工作区根内）。' },
  edit_file: { platforms: { windows: true, linux: true, mobile: true }, note: '移动端写入受沙箱目录限制（仅工作区根内）。' },
  browser_probe: { mode: '按需下发', deps: ['chromium'], platforms: { windows: true, linux: true, mobile: false }, note: '探测本机无头浏览器能力；仅 PC 端可安装，移动端走服务端代理。' },
  browser_text: { mode: '按需下发', deps: ['chromium'], platforms: { windows: true, linux: true, mobile: false }, note: '无头打开 URL 取渲染后文本快照（一期仅文本，不含点击/截图）；仅 PC 端可安装。' },
  save_image: { mode: '按需下发', deps: ['fs_write'], platforms: { windows: true, linux: true, mobile: true }, note: '仅落盘校验（魔数 + 尺寸），不生成图片。' },
  read_image_meta: { mode: '按需下发', deps: ['fs_read'], platforms: { windows: true, linux: true, mobile: true }, note: '只读元信息，不经像素识别。' },
  skill_save: { platforms: { windows: true, linux: true, mobile: false }, note: '技能沉淀需本地文件写入与脚本运行时，移动端仅支持安装现成技能。' },
}

export function toolInstallInfo(name: string, group: ToolGroup): ToolInstallMeta {
  const base = toolInstallMeta[group]
  const o = toolInstallOverrides[name]
  return o
    ? {
        ...base,
        mode: o.mode ?? base.mode,
        deps: o.deps ?? base.deps,
        platforms: o.platforms ?? base.platforms,
        note: o.note ?? base.note,
      }
    : base
}
