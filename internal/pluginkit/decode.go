package pluginkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"harness-go/pkg/toolapi"
)

// DecodeArgs 把调用参数解码到结构体。
//
// # 为什么必须是严格模式（本次改造的核心之一）
//
// 改造前这里用的是裸 json.Unmarshal：**未知字段被静默丢弃**。
// 这与提示词里"工具名/参数名可能漂移"叠加后，产生了最难排查的一类故障：
//
//	模型按提示词传 {"offset_line": 8, "max_lines": 2}
//	实现读的是 {"offset": ..., "limit": ...}
//	→ 不报错、不告警，参数全部取默认值
//	→ 表现为"分页失效""过滤没生效"，看起来像功能坏了，实则契约对不上
//
// 严格模式把这种"静默降级"变成显式错误，并直接告诉模型**可用的字段名**，
// 使一次失败的调用能自我纠正，而不是反复无效重试。
//
// # 为什么错误信息要带上可用字段
//
// 只说 `unknown field "offset"` 对模型帮助有限。附上可用字段清单后，
// 模型能在下一轮直接改用正确名字，避免消耗多轮试错（每轮都是真实 token）。
func DecodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil // 空参数合法（全默认值工具）
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := unknownFieldName(err); ok {
			return fmt.Errorf("%w: 未知参数 %q（可用参数：%s）。请勿猜测参数名，可用 lookup_tool 查询该工具的完整参数规范",
				toolapi.ErrInvalidArgs, field, availableFields(v))
		}
		return fmt.Errorf("%w: 参数解析失败: %v", toolapi.ErrInvalidArgs, err)
	}
	return nil
}

// unknownFieldName 从 Decoder 错误里取出未知字段名。
//
// 标准库的错误格式固定为 `json: unknown field "xxx"`；这里不依赖其完整
// 语义，只做一次前缀+引号提取，取不到就返回 false 走通用错误路径
// （绝不因为信息格式变化而把真实错误吞掉）。
func unknownFieldName(err error) (string, bool) {
	const marker = `unknown field `
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimSpace(msg[i+len(marker):])
	rest = strings.TrimPrefix(rest, `"`)
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j], true
	}
	return "", false
}

// availableFields 返回目标结构体可接受的 JSON 字段名（升序）。
//
// 只处理「指向结构体的指针」这一种形态：所有工具的参数都是这个形状。
// 非结构体返回 "?"，保证错误信息不会因此退化为空。
func availableFields(v any) string {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return "?"
	}
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "（无）"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
