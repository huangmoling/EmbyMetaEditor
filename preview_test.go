package main

import (
	"strings"
	"testing"
)

// formatFieldValue 要能吃下**两种来源**的字段值：
//   - 来自 Emby 的 JSON：`[]any` / `map[string]any`（解码后的通用形态）
//   - 来自我们自己拼的 patch：`[]string` / `[]map[string]any`
//
// 少了任何一种，预演的「现值 vs 新值」就会有一边显示成 `[map[Name:...]]`
// 这种 Go 语法，用户根本看不懂。
func TestFormatFieldValueForms(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"字符串", "标题", "标题"},
		{"我们的 []string", []string{"甲", "乙"}, "甲；乙"},
		{"Emby 的 []any 字符串", []any{"甲", "乙"}, "甲；乙"},
		{"Emby 的 []any 对象", []any{
			map[string]any{"Name": "演员A"},
			map[string]any{"Name": "演员B"},
		}, "演员A；演员B"},
		{"我们的 []map[string]any", []map[string]any{
			{"Name": "片商"},
		}, "片商"},
		{"空数组", []string{}, ""},
		{"没有 Name 的对象", map[string]any{"Id": "x"}, "map[Id:x]"},
	}
	for _, c := range cases {
		if got := formatFieldValue(c.in); got != c.want {
			t.Errorf("%s: formatFieldValue(%#v) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// 简介动辄几千字，预演结果会跟着 job 一起回给前端 —— 不截断的话一次批量就是几 MB。
func TestFormatFieldValueTruncates(t *testing.T) {
	long := strings.Repeat("a", previewValueLimit+137)
	got := formatFieldValue(long)
	if len(got) != previewValueLimit+len("…") {
		t.Errorf("长度 = %d，期望 %d（%d 字节 + 省略号）", len(got), previewValueLimit+len("…"), previewValueLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应以省略号结尾：%q", got[len(got)-10:])
	}
	// 刚好到上限不该被截断
	exact := strings.Repeat("b", previewValueLimit)
	if got := formatFieldValue(exact); got != exact {
		t.Errorf("刚好 %d 字节不该截断，实际长度 %d", previewValueLimit, len(got))
	}
}

// previewPatch 的顺序必须稳定：map 遍历顺序是随机的，不排序的话同一个条目
// 两次预演的行序都不一样，看着像数据在变。
func TestPreviewPatchSortedStable(t *testing.T) {
	item := Item{
		"Id":             "m1",
		"Name":           "旧标题",
		"Overview":       "旧简介",
		"PremiereDate":   "2020-01-01",
		"ProductionYear": 2020,
	}
	patch := map[string]any{
		"PremiereDate":   "2021-04-06",
		"Name":           "新标题",
		"Overview":       "旧简介", // 与现值相同 → Same
		"ProductionYear": 2021,
	}

	got := previewPatch(item, patch)
	if len(got) != len(patch) {
		t.Fatalf("字段数 = %d，期望 %d", len(got), len(patch))
	}
	// 按字段名升序，且**两次调用的顺序完全一致**
	wantOrder := []string{"Name", "Overview", "PremiereDate", "ProductionYear"}
	for i, w := range wantOrder {
		if got[i].Field != w {
			t.Errorf("第 %d 项 = %q，期望 %q（应字段名升序）", i, got[i].Field, w)
		}
	}
	again := previewPatch(item, patch)
	for i := range got {
		if got[i] != again[i] {
			t.Errorf("两次预演结果不一致：%+v vs %+v", got[i], again[i])
		}
	}

	byField := map[string]FieldChange{}
	for _, c := range got {
		byField[c.Field] = c
	}
	if c := byField["Name"]; c.Before != "旧标题" || c.After != "新标题" || c.Same {
		t.Errorf("Name: 期望 旧标题 → 新标题 且不等，实际 %+v", c)
	}
	// 「这条其实不会变」本身是有用信息（能看出某个源没给出新东西），
	// 所以 Same 的条目要**照样列出来**，只是 Same=true 让界面灰掉。
	if c := byField["Overview"]; !c.Same {
		t.Errorf("Overview 现值与新值相同，应标记 Same，实际 %+v", c)
	}
}

// Emby 返回的 JSON 解出来是 `[]any`+`map[string]any`，我们构造的 patch 里是
// `[]map[string]any` —— 两边其实是同一份数据，必须判为「不变」，
// 否则每个条目都会在预演里显示一堆假的「改动」。
func TestPreviewPatchSameAcrossShapes(t *testing.T) {
	item := Item{
		"Studios": []any{map[string]any{"Name": "S1"}, map[string]any{"Name": "S2"}},
		"Tags":    []any{"甲", "乙"},
	}
	patch := map[string]any{
		"Studios": []map[string]any{{"Name": "S1"}, {"Name": "S2"}},
		"Tags":    []string{"甲", "乙"},
	}
	for _, c := range previewPatch(item, patch) {
		if !c.Same {
			t.Errorf("%s 两边数据等价，应判为 Same，实际 Before=%q After=%q", c.Field, c.Before, c.After)
		}
	}
}

func TestChangedFields(t *testing.T) {
	changes := []FieldChange{
		{Field: "A", Same: true},
		{Field: "B"},
		{Field: "C", Same: true},
		{Field: "D"},
	}
	got := changedFields(changes)
	if strings.Join(got, ",") != "B,D" {
		t.Errorf("changedFields = %v，期望 [B D]", got)
	}
	if len(changedFields(nil)) != 0 {
		t.Error("空输入应返回空切片")
	}
}
