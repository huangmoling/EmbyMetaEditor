package main

import "testing"

// 磁力列表按体积倒序：一组覆盖各单位的换算表。
//
// 为什么单独测换算：`1.5GB` 和 `980MB` 用字符串比大小是错的（"9" > "1"），
// 必须先换成字节数。而且真实页面上大小偶尔取不到（结构变动、`—` 占位），
// 这种情况必须明确「排最后」而不是当成 0 混进中间。
func TestMagnetSizeBytes(t *testing.T) {
	// 走函数调用而不是常量表达式：`int64(1.83 * float64(1<<30))` 会被当成
	// 常量转换，Go 直接拒绝（不是整数值），必须让它在运行期算。
	gb := func(f float64) int64 { return int64(f * float64(int64(1)<<30)) }
	mb := func(n int64) int64 { return n << 20 }
	cases := []struct {
		in   string
		want int64
	}{
		{"1TB", 1 << 40},
		{"1.83GB", gb(1.83)},
		{"2.57 GB", gb(2.57)}, // 页面上有带空格的写法
		{"500MB", mb(500)},
		{"1024KB", 1024 << 10},
		{"0B", 0},
		{"", -1},
		{"—", -1},
		{"未知", -1},
		{"GB", -1},
	}
	for _, c := range cases {
		if got := magnetSizeBytes(c.in); got != c.want {
			t.Errorf("magnetSizeBytes(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
	// 大小关系不能反：这是排序正确的前提。
	// 980MB < 1.2GB 是字符串比大小会判错的那个例子（"9" > "1"）。
	if !(magnetSizeBytes("1TB") > magnetSizeBytes("1.2GB") &&
		magnetSizeBytes("1.2GB") > magnetSizeBytes("980MB") &&
		magnetSizeBytes("980MB") > magnetSizeBytes("700MB")) {
		t.Error("单位换算的大小关系不对：980MB 不应大于 1.2GB")
	}
}

func TestSortMagnetsBySizeDesc(t *testing.T) {
	mags := []JBMagnet{
		{Link: "a", Name: "A", Size: "700MB"},
		{Link: "b", Name: "B", Size: "12.3GB"},
		{Link: "c", Name: "C", Size: ""}, // 取不到体积
		{Link: "d", Name: "D", Size: "2.57GB"},
		{Link: "e", Name: "E", Size: "—"}, // 取不到体积
	}
	sortMagnetsBySize(mags)
	got := make([]string, len(mags))
	for i, m := range mags {
		got[i] = m.Name
	}
	// c / e 体积相同（都排不了）时要保持原有相对顺序 —— 用 SliceStable，
	// 否则每次刷新顺序都在跳，用户会以为列表变了。
	want := []string{"B", "D", "A", "C", "E"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序结果 %v，期望 %v", got, want)
		}
	}
}

// 用**线上真实形态**的夹具跑一遍「解析 → 组装」，确认排序真的接上了。
//
// 这条是防「排序没接上」的：如果只测 sortMagnetsBySize 本身，
// 把它从组装路径里删掉测试照样全绿（典型的空过）。
func TestMagnetResultFromSortsRealFixture(t *testing.T) {
	mags := parseMagnets([]byte(magnetAjaxBareFixture))
	if len(mags) != 2 {
		t.Fatalf("夹具应解析出 2 条磁力，实际 %d", len(mags))
	}
	// 解析必须保持页面上的原始顺序（1.83GB 在前）—— 排序是之后的事，
	// 两者混在一起的话，解析层的测试就会被排序策略的改动带崩。
	if mags[0].Size != "1.83GB" || mags[1].Size != "2.57GB" {
		t.Fatalf("解析应保持文档顺序，实际 %q / %q", mags[0].Size, mags[1].Size)
	}

	out := magnetResultFrom("SSNI-989", "https://www.javbus.com/SSNI-989", "SSNI-989",
		&JBMovie{Number: "SSNI-989", Title: "标题", Magnets: mags})
	if out.Error != "" {
		t.Fatalf("不该报错：%s", out.Error)
	}
	if len(out.Magnets) != 2 {
		t.Fatalf("应带出 2 条磁力，实际 %d", len(out.Magnets))
	}
	if out.Magnets[0].Size != "2.57GB" || out.Magnets[1].Size != "1.83GB" {
		t.Errorf("组装后应按体积倒序，实际 %q / %q", out.Magnets[0].Size, out.Magnets[1].Size)
	}
	// 不该改动调用方手里那份解析结果
	if mags[0].Size != "1.83GB" {
		t.Errorf("排序不应就地改动入参，实际 %q", mags[0].Size)
	}
}

// 没有磁力时要有明确的说明文案，而不是一个空列表让界面显示「没有磁力链接」之外的东西。
func TestMagnetResultFromEmpty(t *testing.T) {
	out := magnetResultFrom("ABC-001", "", "", &JBMovie{Number: "ABC-001"})
	if out.Error != "该作品暂无磁力链接" {
		t.Errorf("空磁力应给出提示，实际 %q", out.Error)
	}
	if len(out.Magnets) != 0 {
		t.Errorf("不应有磁力条目，实际 %d", len(out.Magnets))
	}
}
