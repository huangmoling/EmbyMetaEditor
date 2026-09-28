package main

// 写入「预演」（dry-run）共用的变更预览。
//
// 为什么要有这个东西：这个工具所有写操作都是**不可逆**的（Emby 的
// `POST /Items/{id}` 是整对象替换，没有历史版本），批量刮削一次几十条，
// 点错了就是几十条被改掉。服务端的 dry-run 早就实现了（`DryRun` 一路透传到
// `ScrapeMovie` / `scrapeCNWith`），但界面从来不传，等于白写。
//
// 这里刻意做成**共用**的一层：预演和真实写入必须走同一份「算出要改什么」的逻辑
// （`buildItemPatch` / `planCN`），否则「预演说改 3 个字段、真写改了 5 个」——
// 预览变成谎话，比没有预览更糟。

import (
	"fmt"
	"sort"
	"strings"
)

// FieldChange 是「这次写入会把这个字段从什么改成什么」的一条。
type FieldChange struct {
	Field  string `json:"field"`  // Emby 的字段名（Name / Overview / Tags …）
	Before string `json:"before"` // 当前值（长文本已截断）
	After  string `json:"after"`  // 这次会写进去的值
	Same   bool   `json:"same"`   // 两边一样 —— 写了也等于没改，界面灰掉
}

// previewValueLimit 是预览里每个值的展示上限。
// 简介动辄几千字，预演结果会跟着 job 一起回给前端，不截断的话一次批量就是几 MB。
const previewValueLimit = 400

// formatFieldValue 把 Emby 的字段值压成一行可读文本。
//
// 注意 Emby 返回的 JSON 解出来是 `[]any` / `map[string]any`（不是 `[]string`），
// 而我们构造的 patch 里是 `[]string` / `[]map[string]any` —— 两种都要认：
// 预演的「现有值」来自 Emby、「新值」来自我们自己拼的 patch。
func formatFieldValue(v any) string {
	s := formatFieldValueRaw(v)
	if len(s) > previewValueLimit {
		s = s[:previewValueLimit] + "…"
	}
	return s
}

func formatFieldValueRaw(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []string:
		return strings.Join(t, "；")
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := formatFieldValueRaw(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "；")
	case []map[string]any:
		parts := make([]string, 0, len(t))
		for _, m := range t {
			if s := formatFieldValueRaw(m); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "；")
	case map[string]any:
		// Studios / People 这类对象数组，展示 Name 就够了
		if n, ok := t["Name"].(string); ok {
			return n
		}
		return fmt.Sprint(t)
	default:
		return fmt.Sprint(v)
	}
}

// previewPatch 把「将要写入的 patch」和条目当前值逐字段比对，产出预览。
//
// 顺序按字段名排序（map 遍历顺序是随机的，不排的话同一个条目两次预演的
// 表格行序都不一样，看着像数据在变）。`Same` 的条目**照样列出来**，只是界面会
// 灰掉 —— 「这条其实不会变」本身就是有用信息（能看出某个源没给出新东西）。
func previewPatch(item Item, patch map[string]any) []FieldChange {
	out := make([]FieldChange, 0, len(patch))
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		before := formatFieldValue(item[k])
		after := formatFieldValue(patch[k])
		out = append(out, FieldChange{Field: k, Before: before, After: after, Same: before == after})
	}
	return out
}

// changedFields 从预览里挑出真正会变的字段名（给日志和汇总用）。
func changedFields(changes []FieldChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		if !c.Same {
			out = append(out, c.Field)
		}
	}
	return out
}
