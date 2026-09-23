package main

// 本地诊断工具：读取 tools/probe 落盘的 .recent_*.json 抓包数据，
// 统计「已刮削条目里标题不含番号」的情况，用来验证番号策略。
// 没有落盘数据时自动跳过 —— 不影响 CI / 他人仓库的测试运行。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// probeDumpFiles 找工作目录里的落盘数据；没有就跳过测试。
func probeDumpFiles(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob(".recent_*.json")
	if len(files) == 0 {
		t.Skip("无 .recent_*.json 落盘数据（本地诊断用），跳过")
	}
	return files
}

func loadProbeItems(t *testing.T) []Item {
	t.Helper()
	byID := map[string]Item{}
	for _, f := range probeDumpFiles(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var dump struct {
			Items []Item `json:"Items"`
		}
		if err := json.Unmarshal(raw, &dump); err != nil {
			t.Fatal(err)
		}
		for _, it := range dump.Items {
			id, _ := it["Id"].(string)
			byID[id] = it
		}
	}
	out := make([]Item, 0, len(byID))
	for _, it := range byID {
		out = append(out, it)
	}
	return out
}

// 已刮削条目里，有多少条 **Name 本身不含番号**（只靠 Path / OriginalTitle 兜住）。
func TestProbeNameWithoutNumber(t *testing.T) {
	items := loadProbeItems(t)
	type row struct{ name, path, prov string }
	rows := []row{}
	scraped := 0
	for _, it := range items {
		pids, _ := it["ProviderIds"].(map[string]any)
		if pids == nil {
			continue
		}
		pv, _ := pids["MetaTube"].(string)
		if pv == "" {
			continue
		}
		scraped++
		name, _ := it["Name"].(string)
		if len(numKeys(name)) > 0 {
			continue
		}
		path, _ := it["Path"].(string)
		rows = append(rows, row{name, path, pv})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	t.Logf("已刮削 %d 条；其中 Name 本身不含番号 %d 条", scraped, len(rows))
	for i, r := range rows {
		if i >= 25 {
			break
		}
		t.Logf("---\nName=%s\nProv=%s\nPath=%s", r.name, r.prov, r.path)
	}
}

// 番号是否只存在于 Tags 里（Emby 这个构建不返回 Tags）。
func TestProbeNumberOnlyInTags(t *testing.T) {
	items := loadProbeItems(t)
	n := 0
	for _, it := range items {
		pids, _ := it["ProviderIds"].(map[string]any)
		if pids == nil {
			continue
		}
		if pv, _ := pids["MetaTube"].(string); pv == "" {
			continue
		}
		for _, k := range []string{"Name", "OriginalTitle", "Path", "SortName"} {
			s, _ := it[k].(string)
			if len(numKeys(s)) > 0 {
				goto next
			}
		}
		n++
	next:
	}
	t.Logf("Name/Orig/SortName/Path 全都算不出番号的已刮削条目：%d", n)

	// Tags 字段到底有没有值
	withTags := 0
	tagSample := []string{}
	for _, it := range items {
		if v, ok := it["Tags"].([]any); ok && len(v) > 0 {
			withTags++
			if len(tagSample) < 8 {
				parts := []string{}
				for _, x := range v {
					parts = append(parts, fmt.Sprint(x))
				}
				tagSample = append(tagSample, strings.Join(parts, " | "))
			}
		}
	}
	t.Logf("Tags 字段非空的条目：%d / %d", withTags, len(items))
	for _, s := range tagSample {
		t.Logf("Tags: %s", s)
	}
}
