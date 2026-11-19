package sop

import (
	"regexp"
	"sort"
	"strings"
)

// 工具引用提取：把提示词文本里的「反引号标识符」抽取出来，供一致性门禁比对。
//
// # 为什么需要这个能力（缺陷背景）
//
// 工具名是**机器契约**，必须与被注册的可执行体逐字符一致；但它却被写在
// 自然语言 SOP / 系统提示词里 —— 编译器看不见、测试也不知道，于是漂移
// 长期潜伏：文本教模型调用 `create_plan`，注册表里却只有 `plan_set`，
// 模型拿到「工具未注册」的软失败，还会自行改试别名，排查成本极高。
//
// 本文件的提取结果是那层「缺失的护栏」：只要文本里出现反引号包裹的
// 标识符，就可以机器化地与真实注册表求差集。
//
// # 为什么不直接扫描 Go 源码
//
// 源码里的字符串字面量带引号、带换行拼接（`lines(...)`）、带转义，
// 用正则抓 `...` 会把 Go 语法噪声（如 json 标签 `json:"name"`）一起抓进来。
// 扫描**运行时真实注入的文本**（Catalog() 的 VariantView.Text、embed 的
// agent-core.md）才是模型真正看到的那份措辞，也天然聚焦。
//
// # 判定口径
//
// 只有「形如工具名」的标识符参与比对：小写字母开头、仅含小写字母/数字/下划线。
// 这样 `.harness/plans/`、`Stop-Process -Name <name>`、`json:"name"`、
// `recognize_image(path, prompt)` 等都会被自然排除（含点、空格、大写、括号）。
var backtickRef = regexp.MustCompile("`([^`\n]+)`")

// toolNameLike 判断一个反引号内文本是否「形如工具名」。
//
// 规则刻意保守：只接受 ^[a-z][a-z0-9_]*$ 且长度 ≥ 3。
// 保守的代价是可能漏掉个别别名，收益是**零误报** —— 门禁一旦误报，
// 就会被加白名单绕过，护栏随即失效（这是比漏检更坏的结局）。
var toolNameLike = regexp.MustCompile(`^[a-z][a-z0-9_]{2,}$`)

// externalCommands 明确建模的「操作系统命令」标识符。
//
// # 为什么需要这个集合（而不是叫白名单）
//
// 提示词里的反引号其实承载**两类**截然不同的东西：
//
//	① 本产品工具名（机器契约，必须与 registry 一致）—— 本门禁要校验的；
//	② 操作系统命令（安全红线条目里被禁止的 shell 命令）—— 与 registry 无关。
//
// 早期尝试只按「小写 + 下划线」形态识别，结果 `killall`（安全红线条目
// 里被明确禁止的 Unix 命令）被误判成"引用了不存在的工具"。
//
// 若此时图省事给它开一个"豁免名单"，护栏就出现了可绕的缺口 —— 任何
// 新漂移都能往里丢一条了事。因此这里不是豁免，而是**语义分类**：
// 这些名字**本来就是命令**，不该被当成工具名来校验。
//
// # 维护约束
//
// 该集合必须保持极小（有测试卡上限）。新增条目只有在「它确实是一条
// 操作系统/shell 命令」时才成立；一旦有人往里塞本产品工具名，门禁即失效。
var externalCommands = map[string]struct{}{
	"killall": {},
	"pkill":   {},
}

// isExternalCommand 判断标识符是否为已建模的操作系统命令。
func isExternalCommand(name string) bool {
	_, ok := externalCommands[name]
	return ok
}

// ExternalCommandCount 返回已建模的操作系统命令数量（供门禁测试卡上限）。
func ExternalCommandCount() int { return len(externalCommands) }

// ExtractToolRefs 从给定文本中抽取「形如工具名」的反引号标识符。
// 返回去重后的有序切片（升序），保证门禁输出稳定可比。
//
// 已建模的操作系统命令（killall / pkill 等）被排除：它们不是本产品工具名，
// 拿它们去和 registry 比对只会制造永远无法消除的假阳性。
func ExtractToolRefs(texts ...string) []string {
	seen := make(map[string]struct{})
	for _, text := range texts {
		for _, m := range backtickRef.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			name := strings.TrimSpace(m[1])
			if !toolNameLike.MatchString(name) {
				continue
			}
			if isExternalCommand(name) {
				continue
			}
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CatalogTexts 返回全部 SOP 变体的正文（即模型真实看到的措辞）。
//
// 取 Body 而非 Title/Description：只有 Body 会进提示词，另外两个字段
// 是界面说明文案，不参与注入，若一并扫描会把「界面怎么描述工具」
// 误当成「提示词怎么教模型」。
func CatalogTexts() []string {
	defs := Definitions()
	out := make([]string, 0, len(defs)*2)
	for _, def := range defs {
		for _, v := range def.Variants {
			out = append(out, v.Body)
		}
	}
	return out
}
