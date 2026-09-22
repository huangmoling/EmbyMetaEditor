package main

import (
	"testing"
)

func TestNumKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"SSIS-001", []string{"SSIS-1", "SSIS1"}},
		{"F:\\桌面\\test\\SSIS-001\\SSIS-001.strm", []string{"SSIS-1", "SSIS1"}},
		{"ABP123", []string{"ABP-123", "ABP123"}},
		{"无番号", nil},
		{"HD-1080P", []string{"HD-1080", "HD1080"}},
	}
	for _, c := range cases {
		got := numKeys(c.in)
		if len(got) != len(c.want) {
			t.Errorf("numKeys(%q) = %v，期望 %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("numKeys(%q)[%d] = %q，期望 %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestCanonAndDisplayNumber(t *testing.T) {
	if got := canonNumber("ssis-001"); got != "SSIS-1" {
		t.Errorf("canonNumber 错误: %q", got)
	}
	if got := canonNumber("IPX00535"); got != "IPX-535" {
		t.Errorf("canonNumber 错误: %q", got)
	}
	if got := canonNumber("随便写的"); got != "" {
		t.Errorf("无番号时应返回空，得到 %q", got)
	}
	if got := displayNumber("SSIS-1"); got != "SSIS-001" {
		t.Errorf("displayNumber 应补零: %q", got)
	}
	if got := displayNumber("IPX-535"); got != "IPX-535" {
		t.Errorf("displayNumber 错误: %q", got)
	}
}

func TestNormNameAndVariants(t *testing.T) {
	if normName("三上 悠亜") != normName("三上悠亜") {
		t.Error("空格应被归一化")
	}
	if normName("ＡＢＣ") != "abc" {
		t.Errorf("全角转半角失败: %q", normName("ＡＢＣ"))
	}
	if normName("Kaname Yûna") != normName("KanameYûna") {
		t.Error("英文名空格应被归一化")
	}
	v := nameVariants("三上悠亜")
	if len(v) == 0 || v[0] != normName("三上悠亜") {
		t.Errorf("nameVariants 首个应为归一化原名: %v", v)
	}
}

func TestImageTagExists(t *testing.T) {
	item := Item{"ImageTags": map[string]any{"Primary": "abc"}}
	if !imageTagExists(item, "Primary") {
		t.Error("应检测到 Primary 图片")
	}
	if imageTagExists(item, "Backdrop") {
		t.Error("不应检测到 Backdrop")
	}
	if imageTagExists(Item{}, "Primary") {
		t.Error("空条目不应检测到图片")
	}
	if imageTagExists(Item{"ImageTags": map[string]any{"Primary": "  "}}, "Primary") {
		t.Error("空白 tag 应视为没有图片")
	}
}

func TestNormalizeDate(t *testing.T) {
	if got := normalizeDate("2021-01-05"); got != "2021-01-05T00:00:00.0000000Z" {
		t.Errorf("日期归一化错误: %q", got)
	}
	if got := normalizeDate(""); got != "" {
		t.Errorf("空日期应为空: %q", got)
	}
	if got := normalizeDate("2021/01/05"); got != "" {
		t.Errorf("非法格式应返回空: %q", got)
	}
}

func TestBuildItemPatch(t *testing.T) {
	mv := &MTMovie{
		ID: "abc", Provider: "FANZA", Number: "SSIS-001",
		Title: "SSIS-001 原标题", TitleZh: "中文标题",
		Actors: []string{"三上悠亜", " "}, Director: "监督A",
		Genres: []string{"剧情"}, Studio: "S1", Label: "S1",
		ReleaseDate: "2021-01-05", Runtime: 120, Score: 4.5,
		Plot: "简介内容",
	}
	p := buildItemPatch(Item{}, mv, "SSIS-001")
	if p["Name"] != "中文标题" {
		t.Errorf("应优先中文标题，得到 %v", p["Name"])
	}
	if p["OriginalTitle"] != "SSIS-001 原标题" {
		t.Errorf("原始标题错误: %v", p["OriginalTitle"])
	}
	if p["PremiereDate"] != "2021-01-05T00:00:00.0000000Z" {
		t.Errorf("首播日期错误: %v", p["PremiereDate"])
	}
	if p["ProductionYear"] != 2021 {
		t.Errorf("年份错误: %v", p["ProductionYear"])
	}
	if p["RunTimeTicks"] != int64(120)*60*10_000_000 {
		t.Errorf("时长 tick 错误: %v", p["RunTimeTicks"])
	}
	people, ok := p["People"].([]map[string]any)
	if !ok || len(people) != 2 {
		t.Fatalf("演员+导演应为 2 人，得到 %v", p["People"])
	}
	if people[0]["Name"] != "三上悠亜" || people[0]["Type"] != "Actor" {
		t.Errorf("演员字段错误: %v", people[0])
	}
	if people[1]["Type"] != "Director" {
		t.Errorf("导演字段错误: %v", people[1])
	}
	pids, ok := p["ProviderIds"].(map[string]any)
	if !ok || pids["MetaTube"] != "FANZA:abc" {
		t.Errorf("ProviderIds 错误: %v", p["ProviderIds"])
	}
	if p["SortName"] != "SSIS-001" {
		t.Errorf("SortName 错误: %v", p["SortName"])
	}
}

func TestSearchKeyword(t *testing.T) {
	item := Item{
		"Name": "SSIS-001 某个片名",
		"Path": "F:\\媒体\\SSIS-001\\SSIS-001.strm",
	}
	if got := searchKeyword(item); got != "SSIS-001" {
		t.Errorf("应从条目推断出番号，得到 %q", got)
	}
	if got := searchKeyword(Item{"Name": "无番号条目"}); got != "无番号条目" {
		t.Errorf("无番号时应回退到名称，得到 %q", got)
	}
}

func TestPickBestMatch(t *testing.T) {
	list := []MTMovie{
		{ID: "1", Number: "SSIS-999", Score: 9.9, Provider: "A"},
		{ID: "2", Number: "SSIS-001", Score: 1.0, Provider: "B"},
	}
	got := pickBestMatch(list, "SSIS-001")
	if got.ID != "2" {
		t.Errorf("番号一致应优先于评分，得到 %v", got)
	}
	got = pickBestMatch(list, "不存在-1")
	if got.ID != "1" {
		t.Errorf("无一致番号时应取评分最高，得到 %v", got)
	}
}

func TestFilterMissingImage(t *testing.T) {
	items := []Item{
		{"Id": "a", "ImageTags": map[string]any{"Primary": "t"}},
		{"Id": "b"},
		{"Id": "c", "ImageTags": map[string]any{}},
	}
	got := filterMissingImage(items)
	if len(got) != 2 {
		t.Fatalf("应筛出 2 个缺图条目，实际 %d", len(got))
	}
	if got[0]["Id"] != "b" || got[1]["Id"] != "c" {
		t.Errorf("筛选结果错误: %v", got)
	}
}

func TestGfriendEntryURL(t *testing.T) {
	e := GfriendEntry{Group: "8-GRAPHIS", File: "三上悠亜-1.jpg?t=1657944780"}
	got := e.URL("https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/")
	want := "https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/Content/8-GRAPHIS/%E4%B8%89%E4%B8%8A%E6%82%A0%E4%BA%9C-1.jpg?t=1657944780"
	if got != want {
		t.Errorf("URL 拼接错误:\n got %q\nwant %q", got, want)
	}
}

func TestGroupZh(t *testing.T) {
	if got := groupZh("y-AVDC"); got != "有码 · AVDC" {
		t.Errorf("groupZh 错误: %q", got)
	}
	if got := groupZh("z-Derekhsu"); got != "综合 · Derekhsu" {
		t.Errorf("groupZh 错误: %q", got)
	}
	if got := groupZh("unknown"); got != "unknown" {
		t.Errorf("未知前缀应原样返回: %q", got)
	}
}

func TestGfriendsTreeCandidates(t *testing.T) {
	got := gfriendsTreeCandidates("https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/Filetree.json")
	if len(got) < 3 {
		t.Fatalf("应给出多个备用地址，实际 %d 个: %v", len(got), got)
	}
	if got[0] != "https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/Filetree.json" {
		t.Errorf("首选地址应保持不变: %q", got[0])
	}
	// 去重校验
	seen := map[string]bool{}
	for _, u := range got {
		if seen[u] {
			t.Errorf("候选地址重复: %q", u)
		}
		seen[u] = true
	}
}

func TestAtoiSafe(t *testing.T) {
	if atoiSafe("2021") != 2021 {
		t.Error("atoiSafe 正常数字失败")
	}
	if atoiSafe("20a21") != 20 {
		t.Error("atoiSafe 应在非法字符处停止")
	}
	if atoiSafe("") != 0 {
		t.Error("atoiSafe 空串应为 0")
	}
}

func TestFmtAndTruncate(t *testing.T) {
	if string(truncateBytes([]byte("abcdef"), 3)) != "abc" {
		t.Error("truncateBytes 失败")
	}
	if string(truncateBytes([]byte("ab"), 5)) != "ab" {
		t.Error("truncateBytes 不应补长")
	}
}
