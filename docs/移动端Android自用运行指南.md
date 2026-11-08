# 文档已迁移

本文件已迁移至移动端工程内，请查看：

- **环境搭建（Flutter / JDK / Android SDK / 模拟器 / 联调）**
  → `D:\WorkSpace\Project\harness_mobile\docs\环境搭建指南.md`

- **Android 自用运行与排障（不上架场景）**
  → `D:\WorkSpace\Project\harness_mobile\docs\Android自用运行指南.md`

保留此占位文件是为了避免旧链接 404。

---

## 为什么迁移

移动端相关文档放在 `harness_mobile/docs/` 更符合工程内聚原则：

- 开发者 clone 移动端仓库即可获得全部上手文档，无需跨仓查找
- 文档与代码同仓演进，改动时不易遗漏同步
- `harness_go/docs/` 保留服务端、协议、跨端协同等**跨仓共享**文档

## harness_go/docs 中仍保留的移动端相关文档

| 文档 | 作用 |
| --- | --- |
| `移动端架构与功能设计.md` | 架构基线（选型/协议/红线/能力申报） |
| `移动端开发实施计划.md` | 开发排期、批次拆解、进度回写 |
| `三端功能矩阵与能力申报对齐表.md` | 三端能力归属 |
| `protocols.md` §6 | 设备协议（移动端与之对齐） |
