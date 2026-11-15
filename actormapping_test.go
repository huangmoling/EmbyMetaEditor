package main

// 姓名映射表（actormapping.go）的用例。
//
// 要守的三件事，按重要性排：
//  1. **内嵌的那份数据真的解析得出来**。这类「数据编译进程序」的设计最容易悄悄坏掉：
//     界面一切正常，只是永远查不到人。所以这里不喂夹具，直接解析内嵌数据、要求条数过万。
//  2. **中文名真的能换成日文名**（这是整个功能的理由），而且换出来的写法彼此能互相认同。
//  3. **撞键的键整条丢掉**，不能「先到先得」—— 认错人比查不到人严重得多。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestActorMappingParsesBuiltinData(t *testing.T) {
	m := newActorMapping()
	entries, keys, ambiguous, errMsg := m.Stats()
	if errMsg != "" {
		t.Fatalf("内嵌的姓名映射表解析失败：%s", errMsg)
	}
	// 数据是编译期定死的：条数掉了就是产物出了问题，不是「上游改了」这么轻的事。
	if entries < 25000 {
		t.Errorf("内嵌映射表只有 %d 条，期望 ≥ 25000（数据没打进来？）", entries)
	}
	if keys <= entries {
		t.Errorf("索引键数 %d 不该少于条目数 %d（每条至少三个写法）", keys, entries)
	}
	// 实测 62,098 个键里只有 3 个撞车。变多说明上游出现了重名条目，值得人看一眼。
	if ambiguous > 10 {
		t.Errorf("歧义键有 %d 个（实测上游只有 3 个），上游数据可能出现重名条目", ambiguous)
	}
	// 真查一次：索引建出来了不等于能查到人（键归一化写错的话，条数照样对）。
	if e, err := m.lookup("三上悠亜"); err != nil || e == nil {
		t.Fatalf("内嵌数据里查不到「三上悠亜」（err=%v）", err)
	}
}

// lookup 是索引自身的查询入口，测试里直接用，绕开 App 的懒创建。
func (m *actorMappingIndex) lookup(name string) (*actorMappingEntry, error) {
	if err := m.ensureLoaded(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.idx[normName(name)], nil
}

// 这个功能的全部理由：Emby 里是中文名，日文站只认日文写法。
func TestActorMappingResolvesChineseToJapanese(t *testing.T) {
	m := newActorMapping()
	e, err := m.lookup("三上悠亚")
	if err != nil {
		t.Fatalf("查映射表失败：%v", err)
	}
	if e == nil {
		t.Fatal("映射表里没有「三上悠亚」，中文名到日文名的翻译整条链路等于不存在")
	}
	if got := e.preferredSearchName(); got != "三上悠亜" {
		t.Errorf("首选搜索名 = %q，期望日文写法「三上悠亜」", got)
	}
	aliases := e.aliasesFor("三上悠亚")
	if !slices.Contains(aliases, "三上悠亜") {
		t.Errorf("别名候选里应当有日文名，实际 %q", aliases)
	}
	// 旧艺名也要在：源站（尤其 Wikipedia）常按旧名建条目
	if !slices.Contains(aliases, "鬼头桃菜") && !slices.Contains(aliases, "鬼頭桃菜") {
		t.Errorf("别名候选里应当有旧艺名（鬼头桃菜 / 鬼頭桃菜），实际 %q", aliases)
	}
	// identity 本身不该作为自己的别名再出现一次（会让计数与告警变噪）
	for _, a := range aliases {
		if normName(a) == normName("三上悠亚") {
			t.Errorf("别名候选里不该出现 identity 本身，实际 %q", aliases)
		}
	}
}

// 简体 / 繁体 / 日文 / 旧艺名四种写法必须落到同一条记录上。
func TestActorMappingAcceptsAllSpellings(t *testing.T) {
	m := newActorMapping()
	spellings := []string{"波多野结衣", "波多野結衣", "酒井愛美", "Yui Hatano"}
	var first *actorMappingEntry
	for _, s := range spellings {
		e, err := m.lookup(s)
		if err != nil {
			t.Fatalf("查 %q 失败：%v", s, err)
		}
		if e == nil {
			t.Errorf("映射表里查不到写法 %q", s)
			continue
		}
		if first == nil {
			first = e
			continue
		}
		if e != first {
			t.Errorf("写法 %q 落到了另一条记录（%q vs %q），同一个人的写法没有连起来",
				s, e.JP, first.JP)
		}
	}
	if first == nil {
		t.Fatal("一个写法都没命中")
	}
	if got := first.preferredSearchName(); got != "波多野結衣" {
		t.Errorf("首选搜索名 = %q，期望「波多野結衣」", got)
	}
}

// 归一化：全角、空格、间隔号、大小写差异都要能命中（Emby 里的名字来源很杂）。
func TestActorMappingNormalization(t *testing.T) {
	m := newActorMapping()
	for _, s := range []string{"三上 悠亜", " 三上悠亚 ", "三上・悠亚", "Ｍｉｋａｍｉ Ｙｕａ"} {
		if e, err := m.lookup(s); err != nil {
			t.Fatalf("查 %q 失败：%v", s, err)
		} else if e == nil {
			t.Errorf("归一化后查不到 %q（全角 / 空格 / 标点差异不算不同的人）", s)
		}
	}
}

// 撞键的键必须整条丢掉，而不是「先到先得」。
func TestActorMappingAmbiguousKeysDropped(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<actor>
  <a zh_cn="甲" zh_tw="甲" jp="甲" keyword="共用别名,甲A" />
  <a zh_cn="乙" zh_tw="乙" jp="乙" keyword="共用别名,乙A" />
</actor>`)
	m := newActorMappingFromBytes(raw)
	entries, keys, ambiguous, errMsg := m.Stats()
	if errMsg != "" {
		t.Fatalf("解析夹具失败：%s", errMsg)
	}
	if entries != 2 || keys != 4 || ambiguous != 1 {
		t.Fatalf("期望 2 条 / 4 个键 / 1 个歧义键，实际 %d / %d / %d", entries, keys, ambiguous)
	}
	// 歧义键两个方向都查不到 —— 猜一个的后果是把别人的资料写进你的 Emby
	if e, _ := m.lookup("共用别名"); e != nil {
		t.Errorf("歧义键不该被认下，实际认成了 %q", e.JP)
	}
	// 但两条记录各自的唯一写法照常可用
	for _, want := range []string{"甲", "甲A", "乙", "乙A"} {
		if e, _ := m.lookup(want); e == nil {
			t.Errorf("唯一键 %q 应当可查", want)
		}
	}
}

// 坏数据不能 panic，也不能静默变成「一条都对不上」。
func TestActorMappingBadData(t *testing.T) {
	// 带 BOM：解析前要剥掉（上游重新生成时可能带上）
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`<?xml version="1.0" encoding="UTF-8"?><actor><a zh_cn="丙" jp="丙" /></actor>`)...)
	m := newActorMappingFromBytes(bom)
	if entries, _, _, errMsg := m.Stats(); entries != 1 || errMsg != "" {
		t.Errorf("带 BOM 的文件应当正常解析，实际 entries=%d errMsg=%q", entries, errMsg)
	}

	for _, tc := range []struct {
		desc string
		raw  []byte
	}{
		{"空文件", nil},
		{"只有空白", []byte("  \n\t ")},
		{"不是 XML", []byte("<html><body>404 Not Found</body></html>")},
		{"XML 截断", []byte(`<actor><a zh_cn="甲" jp="甲"`)},
	} {
		m := newActorMappingFromBytes(tc.raw)
		entries, _, _, errMsg := m.Stats()
		if errMsg == "" {
			t.Errorf("%s：应当报错（否则表现为「界面正常、永远查不到人」），实际 entries=%d", tc.desc, entries)
		}
		if !strings.Contains(errMsg, "actor_mapping.xml") {
			t.Errorf("%s：错误信息要指向源文件，实际 %q", tc.desc, errMsg)
		}
	}
}

// App 层：查头像时的候选写法，原名在前、映射表的写法在后。
func TestMappingNameCandidatesForAvatar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a := &App{store: store}

	got := a.mappingNameCandidates("三上悠亚")
	if len(got) < 3 {
		t.Fatalf("候选写法太少：%q", got)
	}
	if got[0] != "三上悠亚" {
		t.Errorf("原名必须排在最前（映射表只是补充），实际 %q", got)
	}
	if !slices.Contains(got, "三上悠亜") {
		t.Errorf("候选里应当有日文名「三上悠亜」，实际 %q", got)
	}
	// 重复写法只出现一次（全角/空格差异归一后也算重复）
	seen := map[string]int{}
	for _, n := range got {
		seen[normName(n)]++
	}
	for k, c := range seen {
		if c > 1 {
			t.Errorf("候选里 %q 出现了 %d 次（应去重）", k, c)
		}
	}
	// 表里没有的人 → 只有原名，不该凭空造出候选
	if only := a.mappingNameCandidates("查无此人測試名"); len(only) != 1 || only[0] != "查无此人測試名" {
		t.Errorf("未命中时应当只返回原名，实际 %q", only)
	}
}

// 映射表的告警通路：表读不出来时必须留一条告警，而不是安静地少一层写法。
func TestMappingFailureSurfacesAsWarning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a := &App{store: store, mappingSrc: newActorMappingFromBytes([]byte("not xml at all"))}
	src := &fakeProfileSource{key: "Fake", label: "假源"}
	prof, err := a.fetchActorProfileWith(context.Background(), []ProfileSource{src},
		"", "三上悠亚", "", FetchOptions{})
	if err != nil {
		t.Fatalf("fetchActorProfileWith: %v", err)
	}
	if len(prof.Warnings) == 0 || !strings.Contains(strings.Join(prof.Warnings, "\n"), "姓名映射表不可用") {
		t.Errorf("映射表坏了必须留告警，实际 %q", prof.Warnings)
	}
	// 坏表不能把抓取搞崩：查询词退回原名，源照常被调用
	if len(src.calls) != 1 || src.calls[0].name != "三上悠亚" {
		t.Errorf("坏表时应当退回原名继续抓，实际 %+v", src.calls)
	}
}

// ---------- 更新通道（内嵌基线 + cache 覆盖）----------

// fixtureMappingXML 造 n 条合法的映射表。
//
// 为什么造这么大的：采用阈值 mappingMinEntries 是一万条，低于它一律回退 ——
// 用 3 条数据测「更新成功」测的是另一条分支，会把阈值这条路整个漏掉。
func fixtureMappingXML(n int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<actor>\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  <a zh_cn=\"测试甲%d\" zh_tw=\"測試甲%d\" jp=\"テスト甲%d\" keyword=\"测试甲%d,TestA%d\" />\n",
			i, i, i, i, i)
	}
	b.WriteString("</actor>\n")
	return []byte(b.String())
}

func TestActorMappingPrefersValidCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	raw := fixtureMappingXML(mappingMinEntries + 5)
	if err := os.WriteFile(actorMappingCachePath(), raw, 0o644); err != nil {
		t.Fatalf("写缓存夹具：%v", err)
	}
	m := newActorMapping()
	entries, _, _, errMsg := m.Stats()
	if errMsg != "" {
		t.Fatalf("解析缓存失败：%s", errMsg)
	}
	if entries != mappingMinEntries+5 {
		t.Errorf("应当用 cache 里那份（%d 条），实际 %d 条", mappingMinEntries+5, entries)
	}
	m.mu.Lock()
	src, at := m.source, m.updatedAt
	m.mu.Unlock()
	if src != mappingSourceCache {
		t.Errorf("source = %q，期望 %q", src, mappingSourceCache)
	}
	if at.IsZero() {
		t.Error("用 cache 时 updatedAt 应当有值（界面要显示更新时间）")
	}
}

func TestActorMappingFallsBackWhenCacheBroken(t *testing.T) {
	for _, tc := range []struct {
		desc string
		raw  []byte
	}{
		{"不是 XML", []byte("<html>404 Not Found</html>")},
		{"条数不够", fixtureMappingXML(10)},
	} {
		dir := t.TempDir()
		t.Setenv("EMBYME_HOME", dir)
		if err := os.WriteFile(actorMappingCachePath(), tc.raw, 0o644); err != nil {
			t.Fatalf("%s：写缓存夹具：%v", tc.desc, err)
		}
		m := newActorMapping()
		entries, _, _, errMsg := m.Stats()
		if errMsg != "" {
			t.Errorf("%s：坏缓存不该让整张表失效（应回退内嵌基线），实际报错 %s", tc.desc, errMsg)
		}
		if entries < 25000 {
			t.Errorf("%s：应当回退到内嵌基线（≥25000 条），实际 %d 条", tc.desc, entries)
		}
		m.mu.Lock()
		src, note := m.source, m.note
		m.mu.Unlock()
		if src != mappingSourceBuiltin {
			t.Errorf("%s：source = %q，期望回退到 %q", tc.desc, src, mappingSourceBuiltin)
		}
		if note == "" {
			t.Errorf("%s：回退必须留下说明，否则用户以为更新过、实际在用旧数据", tc.desc)
		}
	}
}

func TestRefreshActorMapping(t *testing.T) {
	newApp := func(t *testing.T) *App {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("EMBYME_HOME", dir)
		store, err := NewStore(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		return &App{store: store}
	}

	// 上游给一份合法数据 → 落盘 + 立即生效
	t.Run("成功", func(t *testing.T) {
		a := newApp(t)
		raw := fixtureMappingXML(mappingMinEntries + 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write(raw)
		}))
		defer srv.Close()
		restore := actorMappingUpstreamURLs
		actorMappingUpstreamURLs = []string{srv.URL}
		defer func() { actorMappingUpstreamURLs = restore }()

		st, err := a.refreshActorMapping(context.Background())
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if st.Source != mappingSourceCache || st.Entries != mappingMinEntries+1 {
			t.Errorf("更新后状态不对：source=%q entries=%d", st.Source, st.Entries)
		}
		if _, err := os.Stat(actorMappingCachePath()); err != nil {
			t.Errorf("应当落盘到 cache（否则重启就变回去）：%v", err)
		}
		// 立即生效：新数据里的名字当场就能查到，不用重启
		if e, _ := a.actorMapping().lookup("测试甲0"); e == nil {
			t.Error("更新后新数据没有立即生效")
		}
		// 原来的名字应当查不到了（确实换成了这一份，而不是叠加）
		if e, _ := a.actorMapping().lookup("三上悠亚"); e != nil {
			t.Error("更新后的索引里不该还有旧数据")
		}
	})

	// 上游给 HTML 错误页 / 截断数据 → 拒绝覆盖，保留现状
	for _, tc := range []struct {
		desc string
		body string
	}{
		{"HTML 错误页", "<html><body>404</body></html>"},
		{"截断的表", string(fixtureMappingXML(5))},
	} {
		t.Run("拒绝-"+tc.desc, func(t *testing.T) {
			a := newApp(t)
			before := a.mappingStatus()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			restore := actorMappingUpstreamURLs
			actorMappingUpstreamURLs = []string{srv.URL}
			defer func() { actorMappingUpstreamURLs = restore }()

			if _, err := a.refreshActorMapping(context.Background()); err == nil {
				t.Fatalf("%s：应当拒绝", tc.desc)
			}
			if _, err := os.Stat(actorMappingCachePath()); err == nil {
				t.Errorf("%s：拒绝时不许落盘（下次启动会读到坏数据）", tc.desc)
			}
			after := a.mappingStatus()
			if after.Source != before.Source || after.Entries != before.Entries {
				t.Errorf("%s：拒绝时应当保留现状，%+v → %+v", tc.desc, before, after)
			}
		})
	}
}

// 映射表提供的写法**不受「用别名记忆」开关影响**：它是编译进程序的静态事实，
// 不是用户确认过的记忆。关掉记忆的人不该因此丢掉「中文名 → 旧艺名」这对写法 ——
// 否则关一个选项抓取能力就掉一截，而界面上看不出原因。
func TestMappingAliasesIgnoreAliasMemoSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a := &App{store: store, aliases: NewAliasStore()}
	src := &fakeProfileSource{key: "Fake", label: "假源"}
	if _, err := a.fetchActorProfileWith(context.Background(), []ProfileSource{src},
		"", "水野朝陽", "", FetchOptions{UseAliasMemo: false}); err != nil {
		t.Fatalf("fetchActorProfileWith: %v", err)
	}
	if len(src.calls) != 1 {
		t.Fatalf("期望假源被调用 1 次，实际 %d 次", len(src.calls))
	}
	// 水野朝陽 这一条在映射表里带着旧艺名「本多翼」
	if !slices.Contains(src.calls[0].aliases, "本多翼") {
		t.Errorf("关掉别名记忆时，映射表的写法仍应作为候选传给源，实际 %q", src.calls[0].aliases)
	}
	// 而且查询词不该被换成中文写法（这一条的 jp 就是「水野朝陽」本身）
	if src.calls[0].name != "水野朝陽" {
		t.Errorf("查询词 = %q，期望原样", src.calls[0].name)
	}
}
