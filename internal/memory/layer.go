// Package memory 实现记忆四层（阶段 D-3 / 多端协同 §6.3）。
//
// 四层语义（注入方式与预算特征各不相同）：
//
//	L0 核心   用户偏好/硬约束/项目身份（≤800 token）—— 常驻全量注入，进稳定前缀
//	L1 项目   技术栈/约定/避坑 —— 注入 3 行摘要，全文按需拉取
//	L2 情景   会话/任务级结论（digest）—— 向量检索主战场，带时间衰减
//	L3 档案   全量历史事实 —— 检索 + path:line 引用
//
// 存储复用 D-1 检索基建：四层全部落主库 retrieval_docs（kind = memory_l0/l1/l2/l3），
// 向量落 retrieval_vectors —— 同一事实禁止重复落库靠余弦相似度判定，四层统一可比。
// 铁律：本包零 store 依赖，只定义端口与算法；store.RetrievalRepo 的方法签名与
// Repo 端口逐一对应，装配点（cmd/harness-go/main.go）直接传入即可，零适配器。
package memory

import "harness-go/internal/retrieval"

// Layer 记忆层（注入策略与检索通道见包注释）。
type Layer int

const (
	// L0 核心常驻层（≤800 token 全量注入）。
	L0 Layer = iota
	// L1 项目层（注入 3 行摘要）。
	L1
	// L2 情景层（向量检索 top-k）。
	L2
	// L3 档案层（检索 + path:line 引用）。
	L3
)

// Valid 判断层是否已知。
func (l Layer) Valid() bool { return l >= L0 && l <= L3 }

// Kind 层对应的索引类别（retrieval_docs.kind）。
func (l Layer) Kind() retrieval.Kind {
	switch l {
	case L0:
		return retrieval.KindMemoryL0
	case L1:
		return retrieval.KindMemoryL1
	case L2:
		return retrieval.KindMemoryL2
	case L3:
		return retrieval.KindMemoryL3
	default:
		return ""
	}
}

// String 层名（日志与可观测用）。
func (l Layer) String() string {
	switch l {
	case L0:
		return "L0核心"
	case L1:
		return "L1项目"
	case L2:
		return "L2情景"
	case L3:
		return "L3档案"
	default:
		return "未知层"
	}
}
