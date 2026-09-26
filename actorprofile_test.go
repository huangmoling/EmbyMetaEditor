package main

// 演员资料抓取的回归测试。
//
// **夹具全部是从真实响应逐字节存下来的**（testdata/actor/*.html），
// 手写/「顺手补全」结构会让测试全绿而线上全挂 —— 见 README 里磁力那个教训。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func loadFixture(t *testing.T, name string) *html.Node {
	t.Helper()
	path := filepath.Join("testdata", "actor", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("夹具缺失 %s（应从真实响应保存，不要手写）：%v", path, err)
	}
	if len(raw) < 1024 {
		t.Fatalf("夹具 %s 只有 %d 字节，像是被截断/占位文件", path, len(raw))
	}
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("解析夹具失败：%v", err)
	}
	return doc
}

func TestParseAVDBFixture(t *testing.T) {
	doc := loadFixture(t, "avdb_wakamiyahono.html")
	f := parseAVDBDoc(doc, "wakamiyahono", "若宮穂乃", 100)
	if f == nil {
		t.Fatal("解析结果不该为空")
	}

	cases := []struct{ name, got, want string }{
		{"出生日期", f.BirthDate, "1996-10-05"},
		{"出身地", f.BirthPlace, "千葉県"},
		{"血液型", f.BloodType, "A型"},
		{"身高", f.Height, "164"},
		{"胸围", f.Bust, "93"},
		{"腰围", f.Waist, "61"},
		{"臀围", f.Hip, "89"},
		{"罩杯", f.Cup, "G"},
		{"ProviderID", f.ProviderID, "wakamiyahono"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q，期望 %q", c.name, c.got, c.want)
		}
	}
	if f.Agency == "" {
		t.Error("事务所应当解析出来")
	}
	if f.Hobby == "" {
		t.Error("趣味・特技应当解析出来")
	}
	if !strings.Contains(f.Summary, "AV女優") {
		t.Errorf("简介没取到，实际 %q", clipRunes(f.Summary, 60))
	}
	// 別名/旧名 那一栏里有好几个按钮，至少要抓到别名
	if len(f.Aliases) == 0 {
		t.Error("别名一个都没解析出来")
	}
	t.Logf("别名 = %v", f.Aliases)
	t.Logf("事务所 = %q / 兴趣 = %q", f.Agency, f.Hobby)
}

func TestParseAVLeagueFixture(t *testing.T) {
	doc := loadFixture(t, "al_actress_38244.html")
	f := parseAVLeagueDoc(doc, "38244", "坂井なな", 100)
	if f == nil {
		t.Fatal("解析结果不该为空")
	}
	// 这一页大量字段是「不明」，必须被 cleanValue 清成空串，
	// 否则会把「不明」两个字写进用户的 Emby。
	for name, got := range map[string]string{
		"身高": f.Height, "胸围": f.Bust, "腰围": f.Waist, "臀围": f.Hip,
		"血型": f.BloodType, "出生地": f.BirthPlace, "出生日期": f.BirthDate,
	} {
		if got != "" {
			t.Errorf("%s 是「不明」，应当被清空，实际 %q", name, got)
		}
	}
	if f.DebutDate != "2025-05-16" {
		t.Errorf("出道日期 = %q，期望 2025-05-16", f.DebutDate)
	}
	if f.MatchedName != "坂井なな" {
		t.Errorf("匹配名 = %q，期望去掉括号读音后的 坂井なな", f.MatchedName)
	}
	if len(f.Aliases) == 0 || f.Aliases[0] != "さかいなな" {
		t.Errorf("应当把 h1 里的读音当别名，实际 %v", f.Aliases)
	}
	if f.ProviderID != "38244" {
		t.Errorf("ProviderID = %q", f.ProviderID)
	}
}

func TestNameMatchScore(t *testing.T) {
	cases := []struct {
		want, got string
		aliases   []string
		min, max  int
		why       string
	}{
		{"若宮穂乃", "若宮穂乃", nil, 100, 100, "完全相同"},
		{"若宮穂乃", "若宮 穂乃", nil, 100, 100, "空格差异应被归一化"},
		{"水户香奈", "水戸かな", []string{"水戸かな"}, 95, 95, "别名命中"},
		{"朝妃りお", "朝妃りおの別名義・プロフィール・全22作品", nil, 100, 100,
			"标题噪声被 cleanCandidateName 削掉后就是精确匹配"},
		{"朝妃りお", "朝妃りお 出演作品一覧", nil, 80, 80, "名字带无法预料的噪声后缀"},
		{"水户香奈", "水户", nil, 0, 0, "源站名字比我们要找的更短 —— 极可能是另一个人，必须拒绝"},
		{"坂井なな", "坂井美桜", nil, 0, 0, "不同的人"},
		{"若宮穂乃", "逢見リカ", nil, 0, 0, "完全无关"},
	}
	for _, c := range cases {
		got := nameMatchScore(c.want, c.got, c.aliases)
		if got < c.min || got > c.max {
			t.Errorf("nameMatchScore(%q, %q) = %d，期望 [%d,%d]（%s）",
				c.want, c.got, got, c.min, c.max, c.why)
		}
	}
	// 阈值必须挡住「短名字」那一档（0 分那两例），否则会写错人
	if minMatchScore < 80 {
		t.Errorf("阈值 %d 太低：会把源站返回的较短名字当成同一个人", minMatchScore)
	}
}

func TestNormDateAndSizes(t *testing.T) {
	for in, want := range map[string]string{
		"1996-10-05": "1996-10-05",
		"1996年10月5日": "1996-10-05",
		"1996/10/05": "1996-10-05",
		"2025年5月16日": "2025-05-16",
		"不明":         "",
		"":           "",
	} {
		if got := normDate(in); got != want {
			t.Errorf("normDate(%q) = %q，期望 %q", in, got, want)
		}
	}
	b, w, h := parseThreeSizes("B93 / W61 / H89")
	if b != "93" || w != "61" || h != "89" {
		t.Errorf("parseThreeSizes 解析错误：%q %q %q", b, w, h)
	}
	if cup := parseCup("B93 ( G カップ) W61 H89"); cup != "G" {
		t.Errorf("parseCup = %q，期望 G", cup)
	}
	if cup := parseCup("B: - / W: - / H: -"); cup != "" {
		t.Errorf("没有罩杯时应当返回空串，实际 %q", cup)
	}
}

// 全角括号占 3 字节，按字节下标切会切出乱码 —— 这个 bug 单测当场抓到过一次。
func TestSplitReading(t *testing.T) {
	r := splitReading("坂井なな（さかいなな）")
	if r.name != "坂井なな" {
		t.Errorf("主名 = %q，期望 坂井なな", r.name)
	}
	if r.reading != "さかいなな" {
		t.Errorf("读音 = %q，期望 さかいなな；出现乱码说明又按字节切了", r.reading)
	}
	if strings.ContainsRune(r.reading, '\uFFFD') {
		t.Errorf("读音里有替换字符，说明是多字节切割：%q", r.reading)
	}
	// 半角括号、没有括号两种情况
	if got := splitReading("若宮穂乃(wakamiya hono)"); got.reading != "wakamiya hono" {
		t.Errorf("半角括号解析失败：%+v", got)
	}
	if got := splitReading("若宮穂乃"); got.name != "若宮穂乃" || got.reading != "" {
		t.Errorf("没有括号时不该乱拆：%+v", got)
	}
}

// 详情页姓名是最终闸门：搜索结果卡片文字只是线索。
func TestConfirmDetailName(t *testing.T) {
	// 详情页确认了就是本人 → 通过，且分数按详情页算
	if score, ok, _ := confirmDetailName("坂井なな", nil, "坂井なな", 80); !ok || score != 100 {
		t.Errorf("详情页精确命中应当通过并给 100，实际 %d/%v", score, ok)
	}
	// 搜索结果像、但详情页是另一个人 → 必须拒绝
	if _, ok, _ := confirmDetailName("坂井なな", nil, "坂井ななせ", 80); ok {
		t.Error("详情页姓名对不上时必须拒绝，否则资料会写到别人头上")
	}
	// 详情页没有姓名元素 → 退回搜索分数
	if score, ok, _ := confirmDetailName("坂井なな", nil, "", 80); !ok || score != 80 {
		t.Errorf("详情页无姓名时应退回搜索分数，实际 %d/%v", score, ok)
	}
	if _, ok, _ := confirmDetailName("坂井なな", nil, "", 0); ok {
		t.Error("搜索分数不够时不该通过")
	}
	// 靠别名在详情页命中
	if score, ok, _ := confirmDetailName("水户香奈", []string{"水戸かな"}, "水戸かな", 0); !ok || score != 95 {
		t.Errorf("别名命中详情页应当通过，实际 %d/%v", score, ok)
	}
}

// 这是整个功能最关键的一条安全性质：Emby 已有值的字段**绝不能被覆盖**。
func TestBuildActorProfileOnlyFillsBlank(t *testing.T) {
	facts := []ActorFacts{{
		Source: "AvDataBank", SourceLabel: "AVデータバンク",
		BirthDate: "1996-10-05", BirthPlace: "千葉県", Height: "164",
		Bust: "93", Waist: "61", Hip: "89", Cup: "G", BloodType: "A型",
		Hobby: "バドミントン、お酒", Agency: "SOD キミホレ",
		ProviderID: "wakamiyahono",
		Aliases:    []string{"わかみやほの"},
		Tags:       []string{"熟女", "単体作品"},
	}}

	t.Run("Emby 全空时全部可写", func(t *testing.T) {
		p := buildActorProfile("若宮穂乃", "p1", facts, embyExisting{})
		if p.WriteCount == 0 {
			t.Fatal("Emby 全空时应当有可写字段")
		}
		for _, f := range p.Fields {
			if f.Value == "" {
				continue
			}
			if !f.WillWrite {
				t.Errorf("字段 %s 抓到了值却没标记为可写：%s", f.Label, f.Note)
			}
		}
		if !strings.Contains(p.Overview, "出生日期：1996-10-05") {
			t.Errorf("结构化简介不对：\n%s", p.Overview)
		}
		// 各源的标签并进简介：Emby 对 Person 不保存 Tags，标签只能挂在这里
		if !strings.Contains(p.Overview, "标签：熟女、単体作品") {
			t.Errorf("标签没并进简介：\n%s", p.Overview)
		}
		// 标签不该再作为独立字段出现（写不进去的字段不该出现在对照表里）
		for _, f := range p.Fields {
			if f.Key == "tags" {
				t.Error("tags 不该再出现在受管字段里")
			}
		}
		if len(p.Aliases) != 1 || p.Aliases[0] != "わかみやほの" {
			t.Errorf("别名收集不对：%v", p.Aliases)
		}
	})

	t.Run("Emby 已有值时一律跳过", func(t *testing.T) {
		ex := embyExisting{
			Overview:   "我自己写的简介",
			Year:       "1990",
			Birth:      "1990-01-01",
			Locations:  "东京",
			ProviderID: "AvDataBank: old-slug",
		}
		p := buildActorProfile("若宮穂乃", "p1", facts, ex)
		if p.WriteCount != 0 {
			var bad []string
			for _, f := range p.Fields {
				if f.WillWrite {
					bad = append(bad, f.Label)
				}
			}
			t.Fatalf("Emby 已有值的字段不该被写入，却要写：%v", bad)
		}
		for _, f := range p.Fields {
			if f.Value != "" && f.EmbyValue != "" && !strings.Contains(f.Note, "跳过") {
				t.Errorf("字段 %s 已有值但提示不是「跳过」：%s", f.Label, f.Note)
			}
		}
	})

	t.Run("部分为空时只写空的那部分", func(t *testing.T) {
		// 只给了简介，其余都空
		ex := embyExisting{Overview: "已有简介"}
		p := buildActorProfile("若宮穂乃", "p1", facts, ex)
		got := map[string]bool{}
		for _, f := range p.Fields {
			got[f.Key] = f.WillWrite
		}
		if got["overview"] {
			t.Error("简介已有值，不该写")
		}
		if !got["premiere_date"] || !got["production_year"] || !got["production_locations"] {
			t.Errorf("空的字段应当可写，实际 %v", got)
		}
	})

	t.Run("没有任何源命中时不产生任何写入", func(t *testing.T) {
		p := buildActorProfile("某人", "p9", nil, embyExisting{})
		if p.WriteCount != 0 {
			t.Errorf("无资料来源时写 %d 个字段，应当为 0", p.WriteCount)
		}
		if len(p.Warnings) == 0 {
			t.Error("应当给出「所有源都没命中」的提示")
		}
	})
}

func TestMergeFactsPriority(t *testing.T) {
	// 前者优先：AVデータバンク 应该盖住 Wikipedia
	facts := []ActorFacts{
		{SourceLabel: "AVデータバンク", BirthDate: "1996-10-05", BloodType: "A型"},
		{SourceLabel: "Wikipedia", BirthDate: "1900-01-01", BirthPlace: "千葉県"},
	}
	m := mergeFacts(facts)
	if m.fields["birth_date"] != "1996-10-05" {
		t.Errorf("应当是排在前面的源胜出，实际 %q", m.fields["birth_date"])
	}
	if m.source["birth_date"] != "AVデータバンク" {
		t.Errorf("字段来源记错了：%q", m.source["birth_date"])
	}
	// 前者没有的字段由后面的源补
	if m.fields["birth_place"] != "千葉県" {
		t.Errorf("后面的源应当补齐缺失字段，实际 %q", m.fields["birth_place"])
	}
}

func TestAliasStoreRemember(t *testing.T) {
	t.Setenv("EMBYME_HOME", t.TempDir())
	a := NewAliasStore()
	a.Remember("水户香奈", []string{"水戸かな", "Mito Kana", "みとかな"}, "已同步:AVデータバンク")

	names := a.NamesFor("水戸かな")
	if len(names) < 3 {
		t.Fatalf("别名记忆没生效：%v", names)
	}
	found := false
	for _, n := range names {
		if n == "水户香奈" {
			found = true
		}
	}
	if !found {
		t.Errorf("从任一别名出发都应当能找到正名，实际 %v", names)
	}

	// 再记一次要合并且不重复
	a.Remember("水户香奈", []string{"水戸かな", "Kana Mito"}, "已同步:其他源")
	if a.Count() != 1 {
		t.Errorf("同一个人的别名不该分成多组，实际 %d 组", a.Count())
	}

	// 落盘后能重新加载
	b := NewAliasStore()
	if !b.LoadFromCache() {
		t.Fatal("别名记忆没能从缓存加载")
	}
	if len(b.NamesFor("水户香奈")) < 4 {
		t.Errorf("重新加载后别名少了：%v", b.NamesFor("水户香奈"))
	}
}

func TestSyncStoreListHidesSnapshots(t *testing.T) {
	t.Setenv("EMBYME_HOME", t.TempDir())
	s := NewSyncStore()
	s.Add(SyncRecord{
		PersonID: "p1", Name: "某人", Changed: []string{"简介"},
		Before: map[string]any{"overview": ""},
		After:  map[string]any{"overview": "新简介"},
	})
	recs := s.List(10)
	if len(recs) != 1 {
		t.Fatalf("历史条数 = %d", len(recs))
	}
	if recs[0].Before != nil || recs[0].After != nil {
		t.Error("列表接口不该带快照本体（省流量）")
	}
	full, ok := s.Get(recs[0].ID)
	if !ok || full.Before == nil {
		t.Error("按 ID 取应当能拿到完整快照")
	}
	s.MarkRolledBack(recs[0].ID)
	if got, _ := s.Get(recs[0].ID); !got.RolledBack {
		t.Error("回滚标记没落上")
	}
}

// 结构化简介的版式对齐原版扩展器：一行一个字段、缺的不留「不明」。
func TestBuildOverviewLayout(t *testing.T) {
	m := mergeFacts([]ActorFacts{{
		SourceLabel: "AVデータバンク",
		Agency:      "SOD キミホレ", AgencySpan: "2018年11月 - 2019年2月",
		BirthDate: "1996-10-05", BirthPlace: "千葉県", Height: "164",
		Bust: "93", Waist: "61", Hip: "89", Cup: "G", BloodType: "A型",
		Hobby: "バドミントン、お酒",
	}})
	lines := buildOverview(m)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"事务所：SOD キミホレ（2018年11月 - 2019年2月）",
		"出生日期：1996-10-05",
		"出生地：千葉県",
		"身高：164 cm",
		"三围：B93 / W61 / H89",
		"罩杯：G 杯",
		"血型：A型",
		"兴趣：バドミントン、お酒",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("简介缺少 %q\n实际：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "不明") {
		t.Errorf("简介里不该出现「不明」：\n%s", joined)
	}
}

// ---------- 批量入参整理 ----------

// TestProfileTargetsKeepsItemsIDs 界面「批量处理当前页」传的是 items（带 ID），
// 服务端必须原样用这些 ID，不该再按名字去查一遍。
func TestProfileTargetsKeepsItemsIDs(t *testing.T) {
	m := newMockEmby(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", DeviceID: "dev1"})

	in := []profileBatchItem{
		{ID: "p-1", Name: "三上悠亜"},
		{ID: "p-2", Name: " 葵つかさ "}, // 名字两侧空白要吃掉
		{ID: "", Name: ""},          // 空条目直接丢
	}
	got, err := profileTargets(context.Background(), e, in, nil, 0, "")
	if err != nil {
		t.Fatalf("profileTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 个目标，实际 %d 个：%+v", len(got), got)
	}
	if got[0].ID != "p-1" || got[1].ID != "p-2" {
		t.Errorf("ID 被改动了：%+v", got)
	}
	if got[1].Name != "葵つかさ" {
		t.Errorf("名字没 trim：%q", got[1].Name)
	}
}

// TestProfileTargetsResolvesNameOnly 只给名字时，必须解析出 Emby 的 ID。
//
// 这是很容易漏掉的一条：applyActorProfile 拿到空 ID 会直接报「缺少演员 ID」，
// 而它前面读 Emby 现有值那步失败后，每个字段都会被判成「空白、将写入」——
// 从任务日志上看像是要成功，实际全部失败。
func TestProfileTargetsResolvesNameOnly(t *testing.T) {
	m := newMockEmby(t)
	m.persons = []Person{
		{Id: "p-1", Name: "三上悠亜"},
		{Id: "p-2", Name: "葵つかさ"},
	}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", DeviceID: "dev1"})

	got, err := profileTargets(context.Background(), e, nil, []string{"三上悠亜", "查无此人"}, 0, "")
	if err != nil {
		t.Fatalf("profileTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望只留下能解析的 1 个，实际 %d 个：%+v", len(got), got)
	}
	if got[0].ID != "p-1" || got[0].Name != "三上悠亜" {
		t.Errorf("解析结果不对：%+v", got[0])
	}
	for _, g := range got {
		if g.ID == "" {
			t.Fatalf("绝不能把空 ID 传下去：%+v", got)
		}
	}
}

// TestProfileTargetsFallsBackToLimit 什么都不给时按 limit + parentID 拉列表。
func TestProfileTargetsFallsBackToLimit(t *testing.T) {
	m := newMockEmby(t)
	m.persons = []Person{
		{Id: "p-1", Name: "A"},
		{Id: "p-2", Name: "B"},
		{Id: "p-3", Name: "C"},
	}
	// ParentId 必须是 Emby 认得的 GUID 短写：mock 照抄了线上行为 ——
	// 非 GUID 一律 500「Unrecognized Guid format.」，而不是返回空列表。
	m.personParent["p-1"] = "502847"
	m.personParent["p-2"] = "502847"
	m.personParent["p-3"] = "502848"
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", DeviceID: "dev1"})

	got, err := profileTargets(context.Background(), e, nil, nil, 1, "502847")
	if err != nil {
		t.Fatalf("profileTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("limit=1 应当只取 1 个，实际 %d 个：%+v", len(got), got)
	}
	if got[0].ID == "" {
		t.Fatalf("空 ID：%+v", got)
	}
	if m.lastParentID != "502847" {
		t.Errorf("ParentId 没传下去，实际收到 %q", m.lastParentID)
	}
}

// TestProfileTargetsEmpty 库里没命中任何条目时返回空切片，让上层给出明确报错，
// 而不是启动一个 0 目标的「成功」任务。
func TestProfileTargetsEmpty(t *testing.T) {
	m := newMockEmby(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", DeviceID: "dev1"})

	got, err := profileTargets(context.Background(), e, nil, []string{"查无此人"}, 0, "")
	if err != nil {
		t.Fatalf("profileTargets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("期望空结果，实际 %+v", got)
	}
}

// ---------- 写回 / 回滚往返 ----------

// newProfileTestApp 组一个只连 mock Emby 的 App。
//
// 数据目录指向临时目录（EMBYME_HOME），**绝不碰真实 config.json / cache/** ——
// 别名记忆和同步历史都会往 cache/ 落盘，用真目录会把用户的数据洗掉。
func newProfileTestApp(t *testing.T, m *mockEmby) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(c *Config) {
		c.EmbyURL = m.srv.URL
		c.Token = "tok-123"
		c.UserID = "u1"
		c.DeviceID = "dev1"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	return &App{store: store, aliases: NewAliasStore(), sync: NewSyncStore()}
}

// profileWithFields 造一份「抓取结果」，fields 与 buildActorProfile 的产出同形。
//
// 关键：**patch 里的值不是从 Field.Value 拿的**，而是回源到 Facts / Overview：
//
//	Overview        <- prof.Overview
//	PremiereDate    <- Facts.BirthDate
//	ProductionYear  <- 从 Facts.BirthDate 取前四位
//	ProductionLocations <- Facts.BirthPlace
//	ProviderIds     <- Facts[i].ProviderID（按 Source 做 key）
//
// Field.Value 只是给界面看的展示串。之前这里只填展示串不填 Facts，写出去的就是
// 空 —— 看着像「写成功但没生效」，很容易误判成产品 bug。
func profileWithFields(personID, name string, willWrite map[string]string) *ActorProfile {
	fact := ActorFacts{Source: "AvDataBank", SourceLabel: "AVデータバンク", MatchScore: 100}
	prof := &ActorProfile{Name: name, PersonID: personID, Sources: []string{"AVデータバンク"}}
	if v, ok := willWrite["premiere_date"]; ok {
		fact.BirthDate = v
	}
	if v, ok := willWrite["production_locations"]; ok {
		fact.BirthPlace = v
	}
	if v, ok := willWrite["provider_ids"]; ok {
		fact.ProviderID = v
	}
	if v, ok := willWrite["overview"]; ok {
		prof.Overview = v
	}
	if v, ok := willWrite["production_year"]; ok && fact.BirthDate == "" {
		fact.BirthDate = v + "-01-01" // 让 yearOf 取得到年份
	}
	prof.Facts = []ActorFacts{fact}

	for _, k := range profileFieldKeys {
		f := ProfileField{Key: k, Label: k}
		if v, ok := willWrite[k]; ok {
			f.Value, f.WillWrite, f.Note = v, true, "将写入"
		} else {
			f.Note = "Emby 已有值，跳过"
		}
		prof.Fields = append(prof.Fields, f)
	}
	return prof
}

// TestApplyProfileFactsOnlyFillsBlank 写入只发生在 WillWrite 的字段上，
// 其余字段连 patch 都不该进 —— 这是「绝不覆盖」最后的兜底（前面的判定错了，
// 这里也必须挡住）。Emby 的 POST /Items/{id} 是整对象替换，多带一个键就是多毁一处。
func TestApplyProfileFactsOnlyFillsBlank(t *testing.T) {
	m := newMockEmby(t)
	m.items["p-1"] = map[string]any{
		"Id": "p-1", "Name": "江上しほ", "Type": "Person",
		"Overview":            "原有简介不许动",
		"PremiereDate":        "1992-03-01T00:00:00.0000000Z",
		"ProductionYear":      float64(1992),
		"ProductionLocations": []any{"日本·熊本県"},
		"ProviderIds":         map[string]any{"MetaTube": "keep-me"},
	}
	a := newProfileTestApp(t, m)

	prof := profileWithFields("p-1", "江上しほ", map[string]string{"production_locations": "熊本県"})
	res, err := a.applyProfileFacts(context.Background(), prof, nil)
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if len(res.Written) != 1 || res.Written[0] != "production_locations" {
		t.Fatalf("只该写入出生地，实际 %+v", res.Written)
	}

	body := m.patched["p-1"]
	if body == nil {
		t.Fatal("mock 没收到写入请求")
	}
	if body["Overview"] != "原有简介不许动" {
		t.Errorf("Overview 被覆盖了：%v", body["Overview"])
	}
	if body["PremiereDate"] != "1992-03-01T00:00:00.0000000Z" {
		t.Errorf("PremiereDate 被覆盖了：%v", body["PremiereDate"])
	}
	if y, _ := body["ProductionYear"].(float64); int(y) != 1992 {
		t.Errorf("ProductionYear 被覆盖了：%v", body["ProductionYear"])
	}
	if pids, _ := body["ProviderIds"].(map[string]any); pids["MetaTube"] != "keep-me" {
		t.Errorf("ProviderIds 被覆盖了：%v", body["ProviderIds"])
	}
	locs, _ := body["ProductionLocations"].([]any)
	if len(locs) != 1 || locs[0] != "熊本県" {
		t.Errorf("出生地没写进去：%v", body["ProductionLocations"])
	}
}

// profileWithFilled 造一份「Emby 里已经有值」的抓取结果：字段同时有 Value（本次抓到）
// 和 EmbyValue（现有），WillWrite 为 false —— 也就是默认策略下会被跳过的那一类。
// 用来验证「已有值也能被主动勾选覆盖」这条需求。
func profileWithFilled(personID, name string, fetched, existing map[string]string) *ActorProfile {
	prof := profileWithFields(personID, name, fetched)
	for i := range prof.Fields {
		f := &prof.Fields[i]
		if v, ok := existing[f.Key]; ok {
			f.EmbyValue = v
			f.WillWrite = false
			f.Note = "Emby 已有值，跳过"
		}
	}
	prof.WriteCount = 0
	for _, f := range prof.Fields {
		if f.WillWrite {
			prof.WriteCount++
		}
	}
	return prof
}

// TestApplyProfileExplicitKeysOverwrite 是需求「已有值可勾选覆盖」的回归。
//
// 两条承诺同时钉住：
//   - 勾中的字段**可以**覆盖 Emby 里已有的值（这是新加的能力）；
//   - 没勾的字段一个字都不能动（这是覆盖功能的安全边界，比新能力更重要）。
func TestApplyProfileExplicitKeysOverwrite(t *testing.T) {
	m := newMockEmby(t)
	m.items["p-1"] = map[string]any{
		"Id": "p-1", "Name": "星野テスト", "Type": "Person",
		"Overview":            "旧简介",
		"PremiereDate":        "1990-01-01T00:00:00.0000000Z",
		"ProductionYear":      float64(1990),
		"ProductionLocations": []any{"旧出生地"},
		"ProviderIds":         map[string]any{"MetaTube": "keep-me"},
	}
	a := newProfileTestApp(t, m)

	prof := profileWithFilled("p-1", "星野テスト",
		map[string]string{"overview": "新简介", "premiere_date": "1995-04-01"},
		map[string]string{"overview": "旧简介", "premiere_date": "1990-01-01"})
	if prof.WriteCount != 0 {
		t.Fatalf("两个字段 Emby 里都有值，默认一个都不该预判为可写，实际 %d", prof.WriteCount)
	}

	// 只勾「简介」，出生日期不勾
	res, err := a.applyProfileFacts(context.Background(), prof, []string{"overview"})
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if len(res.Written) != 1 || res.Written[0] != "overview" {
		t.Fatalf("只该写入勾中的简介，实际 %+v", res.Written)
	}
	if len(res.Overwritten) != 1 || res.Overwritten[0] != "overview" {
		t.Fatalf("勾中的字段原本有值，应被标成覆盖，实际 %+v", res.Overwritten)
	}
	if !strings.Contains(res.Message, "覆盖") {
		t.Errorf("结果说明里要讲清覆盖了几项，实际 %q", res.Message)
	}

	body := m.patched["p-1"]
	if body == nil {
		t.Fatal("mock 没收到写入请求")
	}
	if body["Overview"] != "新简介" {
		t.Errorf("勾中的简介没被覆盖：%v", body["Overview"])
	}
	// 没勾的字段必须原样保留
	if body["PremiereDate"] != "1990-01-01T00:00:00.0000000Z" {
		t.Errorf("没勾的出生日期被动了：%v", body["PremiereDate"])
	}
	if y, _ := body["ProductionYear"].(float64); int(y) != 1990 {
		t.Errorf("没勾的年份被动了：%v", body["ProductionYear"])
	}
	if locs, _ := body["ProductionLocations"].([]any); len(locs) != 1 || locs[0] != "旧出生地" {
		t.Errorf("没勾的出生地被动了：%v", body["ProductionLocations"])
	}
	if pids, _ := body["ProviderIds"].(map[string]any); pids["MetaTube"] != "keep-me" {
		t.Errorf("没勾的外部 ID 被动了：%v", body["ProviderIds"])
	}
}

// 批量路径（keys 为空）必须还是「只填空白」—— 新加的覆盖能力不能把它带偏。
// 库里上千个演员大多已经有资料，批量跑一遍全量覆盖会毁数据。
func TestApplyProfileNoKeysKeepsOnlyBlankPolicy(t *testing.T) {
	m := newMockEmby(t)
	m.items["p-1"] = map[string]any{
		"Id": "p-1", "Name": "星野テスト", "Type": "Person",
		"Overview": "旧简介",
	}
	a := newProfileTestApp(t, m)

	prof := profileWithFilled("p-1", "星野テスト",
		map[string]string{"overview": "新简介"},
		map[string]string{"overview": "旧简介"})
	res, err := a.applyProfileFacts(context.Background(), prof, nil)
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if len(res.Written) != 0 {
		t.Fatalf("批量模式不该写任何字段，实际 %+v", res.Written)
	}
	if m.patched["p-1"] != nil {
		t.Error("批量模式不该发出任何写入请求")
	}
	if len(res.Skipped) == 0 {
		t.Error("跳过的字段要有说明，否则用户以为抓取失败了")
	}
	if m.items["p-1"]["Overview"] != "旧简介" {
		t.Errorf("Emby 里的原值被改动了：%v", m.items["p-1"]["Overview"])
	}
}

// 覆盖也要能回滚 —— 这是用户敢用覆盖的前提。
func TestOverwriteThenRollbackRestoresOriginal(t *testing.T) {
	m := newMockEmby(t)
	original := map[string]any{
		"Id": "p-1", "Name": "星野テスト", "Type": "Person",
		"Overview":            "用户手写的旧简介",
		"PremiereDate":        "1990-01-01T00:00:00.0000000Z",
		"ProductionYear":      float64(1990),
		"ProductionLocations": []any{"旧出生地"},
		"ProviderIds":         map[string]any{"MetaTube": "keep-me"},
	}
	m.items["p-1"] = original
	a := newProfileTestApp(t, m)

	prof := profileWithFilled("p-1", "星野テスト",
		map[string]string{"overview": "新简介", "production_locations": "新出生地"},
		map[string]string{"overview": "用户手写的旧简介", "production_locations": "旧出生地"})
	res, err := a.applyProfileFacts(context.Background(), prof, []string{"overview", "production_locations"})
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if len(res.Overwritten) != 2 || len(res.Written) != 2 {
		t.Fatalf("两个字段都该被覆盖：written=%v overwritten=%v", res.Written, res.Overwritten)
	}
	if res.RecordID == "" {
		t.Fatal("覆盖写入同样要留快照，否则回滚无从下手")
	}
	if m.items["p-1"]["Overview"] != "新简介" {
		t.Fatalf("覆盖没生效：%v", m.items["p-1"]["Overview"])
	}

	if _, err := a.rollbackSync(context.Background(), res.RecordID); err != nil {
		t.Fatalf("rollbackSync: %v", err)
	}
	after := m.items["p-1"]
	for _, k := range []string{"Overview", "PremiereDate", "ProductionYear", "ProductionLocations", "ProviderIds"} {
		if diff := jsonDiff(original[k], after[k]); diff != "" {
			t.Errorf("回滚后 %s 没还原：%s\n  期望 %#v\n  实际 %#v", k, diff, original[k], after[k])
		}
	}
}

// TestApplyThenRollbackRestores 是这套功能的**核心安全承诺**：
// 写完能一键还原成写入前的样子，一个字段都不差。
func TestApplyThenRollbackRestores(t *testing.T) {
	m := newMockEmby(t)
	original := map[string]any{
		"Id": "p-1", "Name": "江上しほ", "Type": "Person",
		"Overview":            "原有简介不许动",
		"PremiereDate":        "1992-03-01T00:00:00.0000000Z",
		"ProductionYear":      float64(1992),
		"ProductionLocations": []any{"日本·熊本県"},
		"Tags":                []any{"原有标签（Person 不保存，仅确认不会被我们动）"},
		"ProviderIds":         map[string]any{"MetaTube": "keep-me"},
	}
	m.items["p-1"] = original
	a := newProfileTestApp(t, m)

	prof := profileWithFields("p-1", "江上しほ", map[string]string{
		"overview":             "新的简介",
		"production_locations": "熊本県",
		"provider_ids":         "AvDataBank: 12345",
		"premiere_date":        "1993-02-20",
	})
	res, err := a.applyProfileFacts(context.Background(), prof, nil)
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if res.RecordID == "" {
		t.Fatal("写入后必须留下历史记录，否则回滚无从谈起")
	}
	if got := m.items["p-1"]["Overview"]; got != "新的简介" {
		t.Fatalf("写入没生效：%v", got)
	}

	if _, err := a.rollbackSync(context.Background(), res.RecordID); err != nil {
		t.Fatalf("rollbackSync: %v", err)
	}
	after := m.items["p-1"]
	for _, k := range []string{"Overview", "PremiereDate", "ProductionYear", "ProductionLocations", "Tags", "ProviderIds"} {
		if diff := jsonDiff(original[k], after[k]); diff != "" {
			t.Errorf("回滚后 %s 没还原：%s\n  期望 %#v\n  实际 %#v", k, diff, original[k], after[k])
		}
	}
}

// TestRollbackTwiceRejected 同一条记录不能回滚两次 —— 第二次拿到的「写入前」
// 已经不是当前状态了，再写一次等于把用户后来手工改的东西抹掉。
func TestRollbackTwiceRejected(t *testing.T) {
	m := newMockEmby(t)
	m.items["p-1"] = map[string]any{"Id": "p-1", "Name": "江上しほ", "Type": "Person"}
	a := newProfileTestApp(t, m)

	prof := profileWithFields("p-1", "江上しほ", map[string]string{"overview": "新简介"})
	res, err := a.applyProfileFacts(context.Background(), prof, nil)
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if _, err := a.rollbackSync(context.Background(), res.RecordID); err != nil {
		t.Fatalf("第一次回滚应当成功：%v", err)
	}
	if _, err := a.rollbackSync(context.Background(), res.RecordID); err == nil {
		t.Error("第二次回滚应当被拒绝")
	}
}

// TestApplyProfileFactsNoBlankNoWrite 全都有值时**一次请求都不发**。
// 批量跑 2000 个演员时，这一条决定了任务会不会把整个库重写一遍。
func TestApplyProfileFactsNoBlankNoWrite(t *testing.T) {
	m := newMockEmby(t)
	m.items["p-1"] = map[string]any{"Id": "p-1", "Name": "江上しほ", "Type": "Person"}
	a := newProfileTestApp(t, m)

	prof := profileWithFields("p-1", "江上しほ", nil)
	res, err := a.applyProfileFacts(context.Background(), prof, nil)
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if len(res.Written) != 0 {
		t.Errorf("不该写入任何字段：%+v", res.Written)
	}
	if m.patched["p-1"] != nil {
		t.Errorf("没有可写字段时不该发写入请求：%#v", m.patched["p-1"])
	}
	if res.RecordID != "" {
		t.Errorf("没写入就不该有历史记录：%s", res.RecordID)
	}
	if len(a.sync.List(0)) != 0 {
		t.Errorf("同步历史应当为空，实际 %d 条", len(a.sync.List(0)))
	}
}

// TestApplyProfileRejectsEmptyID 空 ID 必须报错而不是「成功」。
func TestApplyProfileRejectsEmptyID(t *testing.T) {
	m := newMockEmby(t)
	a := newProfileTestApp(t, m)
	prof := profileWithFields("", "某人", map[string]string{"overview": "x"})
	if _, err := a.applyProfileFacts(context.Background(), prof, nil); err == nil {
		t.Error("空 ID 应当报错")
	}
}

// jsonDiff 粗略比较两个解码出来的 JSON 值；相等返回空串。
func jsonDiff(want, got any) string {
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if string(wb) == string(gb) {
		return ""
	}
	return "不一致"
}

// TestRollbackClearsFieldsThatWereAbsent 覆盖「写入前那个字段**根本不存在**」的回滚。
//
// 这是线上实测踩到的：toStringSlice 对缺失的值返回**类型化 nil**，而
// `patch[k] == nil` 抓不到它，于是兜底不生效、整个键又被 updateItem 的 nil 防护
// 跳过 —— 结果回滚后端点上「已还原」，字段其实还在。mock 里字段一定存在，
// 所以这条必须专门造一个「快照里没有该键」的场景。
func TestRollbackClearsFieldsThatWereAbsent(t *testing.T) {
	m := newMockEmby(t)
	// 故意不设 ProductionLocations / ProviderIds
	m.items["p-1"] = map[string]any{
		"Id": "p-1", "Name": "玉木くるみ", "Type": "Person",
		"Overview": "原有简介",
	}
	a := newProfileTestApp(t, m)

	prof := profileWithFields("p-1", "玉木くるみ", map[string]string{
		"production_locations": "神奈川県",
		"provider_ids":         "AvDataBank: tamakikurumi",
	})
	res, err := a.applyProfileFacts(context.Background(), prof, nil)
	if err != nil {
		t.Fatalf("applyProfileFacts: %v", err)
	}
	if locs, _ := m.items["p-1"]["ProductionLocations"].([]any); len(locs) != 1 {
		t.Fatalf("写入没生效：%v", m.items["p-1"]["ProductionLocations"])
	}

	if _, err := a.rollbackSync(context.Background(), res.RecordID); err != nil {
		t.Fatalf("rollbackSync: %v", err)
	}
	body := m.patched["p-1"]
	if _, present := body["ProductionLocations"]; !present {
		t.Error("回滚请求里少了 ProductionLocations —— 清空这类字段必须明确发空数组，不能省略")
	}
	if _, present := body["ProviderIds"]; !present {
		t.Error("回滚请求里少了 ProviderIds")
	}
	if locs, _ := m.items["p-1"]["ProductionLocations"].([]any); len(locs) != 0 {
		t.Errorf("回滚后出生地没被清掉：%v", m.items["p-1"]["ProductionLocations"])
	}
	if pids, _ := m.items["p-1"]["ProviderIds"].(map[string]any); len(pids) != 0 {
		t.Errorf("回滚后外部 ID 没被清掉：%v", m.items["p-1"]["ProviderIds"])
	}
}
