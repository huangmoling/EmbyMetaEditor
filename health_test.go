package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLooksLikeFilename(t *testing.T) {
	yes := []string{
		"hhd800.com@91CM014.mp4",
		"SSIS-001.mkv",
		"ABC_123.TS", // 大小写不敏感
		"[FHD]IPX-777.rmvb",
		"xxx.net@SSNI-989.avi",
	}
	for _, s := range yes {
		if !looksLikeFilename(s) {
			t.Errorf("%q 应判为文件名", s)
		}
	}
	no := []string{
		"SSIS-001 某个片名",
		"日本街头拜金女大测试",
		"",
		"1.5 倍速人生",              // 片名里带点但不是扩展名
		"email@example.com 的故事", // @ 后面不是域名结尾的水印形态
	}
	for _, s := range no {
		if looksLikeFilename(s) {
			t.Errorf("%q 不该判为文件名", s)
		}
	}
}

func TestItemTagsOfReadsBothShapes(t *testing.T) {
	it := Item{
		"Tags": []any{"甲", "", "乙"},
		"TagItems": []any{
			map[string]any{"Name": "乙"},
			map[string]any{"Name": "丙"},
			map[string]any{"Id": "no-name"},
		},
	}
	got := itemTagsOf(it)
	if strings.Join(got, ",") != "甲,乙,丙" {
		t.Errorf("tags = %v，期望去重后的 [甲 乙 丙]", got)
	}
	if len(itemTagsOf(Item{})) != 0 {
		t.Error("没有标签时应返回空切片")
	}
}

// 一条条目可以同时命中多个问题；报告要有计数、要有明细、要能算出「干净」的条数。
func TestBuildHealthReportGroupsAndCounts(t *testing.T) {
	items := []Item{
		// 一身的病：没番号、标题是文件名、没海报、没简介、没年份、没标签。
		//
		// 名字刻意用纯中文 + 扩展名：`hhd800.com@xxx.mp4` 那种水印会被
		// `itemNumber` 当成番号（它认「字母+数字」，于是抽出 "hhd800"），
		// 那样这条就不会进「认不出番号」分组。纯粹的中文名没有数字可抽。
		{"Id": "a", "Name": "日本街头拜金女大测试.mp4", "Path": "/x/a.mp4",
			"ImageTags": map[string]any{}, "ProviderIds": map[string]any{}},
		// 只有缺海报
		{"Id": "b", "Name": "SSIS-001 片名", "Path": "/x/b.mp4",
			"Overview": "有简介", "ProductionYear": float64(2021),
			"Tags": []any{"SSIS-001"}, "ImageTags": map[string]any{}},
		// 完全干净
		{"Id": "c", "Name": "SSIS-002 片名", "Path": "/x/c.mp4",
			"Overview": "有简介", "ProductionYear": float64(2021),
			"Tags":      []any{"SSIS-002"},
			"ImageTags": map[string]any{"Primary": "t", "Thumb": "h"}},
	}
	rep := buildHealthReport("lib-1", items, 10)

	if rep.Parent != "lib-1" || rep.Scanned != 3 || rep.Total != 10 {
		t.Errorf("报告元信息不对：%+v", rep)
	}
	if !rep.Truncated {
		t.Error("扫了 3 条但库里共 10 条，应标记 truncated")
	}
	if rep.Clean != 1 {
		t.Errorf("干净条数 = %d，期望 1", rep.Clean)
	}
	if rep.Problem != 2 {
		t.Errorf("有问题条数 = %d，期望 2", rep.Problem)
	}

	byKey := map[string]healthGroup{}
	for _, g := range rep.Groups {
		byKey[g.Key] = g
	}
	if len(rep.Groups) != len(healthIssueDefs) {
		t.Fatalf("分组数 = %d，期望 %d", len(rep.Groups), len(healthIssueDefs))
	}
	if byKey["no_number"].Count != 1 {
		t.Errorf("认不出番号 = %d，期望 1", byKey["no_number"].Count)
	}
	if byKey["title_is_filename"].Count != 1 {
		t.Errorf("标题像文件名 = %d，期望 1", byKey["title_is_filename"].Count)
	}
	if byKey["missing_poster"].Count != 2 {
		t.Errorf("缺海报 = %d，期望 2（a 和 b）", byKey["missing_poster"].Count)
	}
	if byKey["missing_overview"].Count != 1 || byKey["no_year"].Count != 1 || byKey["no_tags"].Count != 1 {
		t.Errorf("缺简介/缺年份/无标签计数不对：%+v", byKey)
	}
	// 第一个分组必须是最严重的（顺序即界面顺序）
	if rep.Groups[0].Level != healthErr {
		t.Errorf("首个分组应为 err 级，实际 %+v", rep.Groups[0])
	}
	// 明细里的 issues 要能反查出这条为什么被列出来
	for _, g := range rep.Groups {
		for _, hi := range g.Items {
			if !cnHasString(hi.Issues, g.Key) {
				t.Errorf("%s 出现在 %s 分组里，但 issues 里没有它：%v", hi.ID, g.Key, hi.Issues)
			}
		}
	}
	// 也给出了算好的番号
	for _, g := range byKey["missing_poster"].Items {
		if g.ID == "b" && g.Number == "" {
			t.Error("明细里应带上算好的番号（列表接口不返回 SortName）")
		}
	}
}

// 分组明细有上限，但计数必须给全 —— 否则用户以为库里只有 100 条缺海报。
func TestBuildHealthReportCapsDetailsButNotCounts(t *testing.T) {
	items := make([]Item, 0, healthGroupItemCap+7)
	for i := 0; i < healthGroupItemCap+7; i++ {
		items = append(items, Item{
			"Id": "x", "Name": "SSIS-001 片名", "Overview": "有简介",
			"ProductionYear": float64(2021), "Tags": []any{"t"},
			"ImageTags": map[string]any{}, // 只缺海报
		})
	}
	rep := buildHealthReport("", items, len(items))
	var poster healthGroup
	for _, g := range rep.Groups {
		if g.Key == "missing_poster" {
			poster = g
		}
	}
	if poster.Count != healthGroupItemCap+7 {
		t.Errorf("计数 = %d，期望 %d（计数不能被上限截断）", poster.Count, healthGroupItemCap+7)
	}
	if len(poster.Items) != healthGroupItemCap {
		t.Errorf("明细条数 = %d，期望上限 %d", len(poster.Items), healthGroupItemCap)
	}
	if poster.More != 7 {
		t.Errorf("More = %d，期望 7", poster.More)
	}
}

// 空库不能崩，也不能报错。
func TestBuildHealthReportEmpty(t *testing.T) {
	rep := buildHealthReport("", nil, 0)
	if rep.Scanned != 0 || rep.Truncated || rep.Clean != 0 {
		t.Errorf("空报告不对：%+v", rep)
	}
	for _, g := range rep.Groups {
		if g.Count != 0 || len(g.Items) != 0 {
			t.Errorf("%s 分组不该有内容：%+v", g.Key, g)
		}
	}
}

// 接口层：必须真的去问 Emby，并且返回结构完整的报告。
func TestHandleHealth(t *testing.T) {
	m := newMockEmby(t)
	app := testApp(t, m.srv.URL, "http://127.0.0.1:1")
	m.libFolders = []LibraryFolder{{Name: "电影", ItemID: "lib-1", Locations: []string{"/media/movies"}}}
	m.mu.Lock()
	m.items["h1"] = map[string]any{
		"Id": "h1", "Name": "hhd800.com@xxx.mp4", "Type": "Movie",
		"Path": "/media/movies/a.mp4", "ParentId": "lib-1",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
	}
	m.mu.Unlock()

	srv := httptest.NewServer(app.route())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/health?parent=lib-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data healthReport `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Parent != "lib-1" {
		t.Errorf("parent = %q", out.Data.Parent)
	}
	if len(out.Data.Groups) == 0 {
		t.Fatal("报告里没有分组")
	}
	// 这条一身都是问题，至少要出现在「标题像文件名」里
	found := false
	for _, g := range out.Data.Groups {
		if g.Key == "title_is_filename" && g.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("应命中「标题像文件名」：%+v", out.Data.Groups)
	}
}
