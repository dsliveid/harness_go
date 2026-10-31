// Package wasm 是 WASM 沙箱插件的**占位目录**（一期不做，D14 决策）。
//
// 为什么一期不做（docs/插件体系设计.md §3.5 四条理由）：
//  1. 隔离目标已被「子进程 + stdio JSON-RPC（复用 MCP 协议）」满足（OS 进程边界），
//     复用度约 80%，新增成本接近零——见 internal/market 与 internal/mcp；
//  2. 执行面已下沉到端侧（HALO）：第三方工具倾向在用户自己的端侧 Runner 运行；
//  3. WASM 真正刚需是「服务端多租户执行不可信代码」，本项目本地单机为一等公民；
//  4. wazero 工程债高（host function / 资源限额 / 调试链路）。
//
// 升级触发条件（满足任一即启动实现，届时本包实现同一 toolapi.Factory 契约）：
//
//	① 上线云端多租户形态；
//	② 工具市场向不可信第三方开放且要求服务端执行。
//
// 契约面已预留：toolapi.KindWasm 枚举存在且合法（Manifest.Validate 接受），
// 将来只需实现 Factory + 用 host function 实现 Host 能力，内核零改动。
package wasm
