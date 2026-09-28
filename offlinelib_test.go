package main

// 离线演员资料库的回归测试。
//
// 这个源的数据来自「另一个工具的加密资料库」→ 由 tools/sqlcipher_dump.py
// 导出成 CSV。所以夹具就是一份 CSV（表头照抄真实导出），不联网、不碰那个 .db。
//
// 夹具**必须走真实的列名与表头**，而不是直接构造 offlineEntry：
// 这条链路上最容易出错的地方就是「列名对不上」（真实的坑：文件带 UTF-8 BOM，
// 第一列会变成 "\ufeffid"，于是所有字段都读成空，而文件看起来完全正常），
// 那种错在直接构造结构体的测试里永远暴露不出来。

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// offlineCSVHeader 是真实导出文件的 41 列表头，顺序也照抄。
// 列顺序与列名都要跟真实文件一致，否则「上游换列」这类问题在测试里照样看不见。
var offlineCSVHeader = []string{
	"id", "name_original", "name_ja", "name_zh_cn", "name_romanized", "kana",
	"nationality", "birthplace", "birthdate", "blood_type",
	"height_cm", "bust_cm", "waist_cm", "hip_cm", "cup", "shoe_cm",
	"body_type", "occupation", "agency", "debut_date", "debut_year",
	"retirement_date", "retirement_year", "career_status", "hobbies", "specialties",
	"biography_original", "biography_zh_cn", "profile_image_url", "official_site",
	"favorite_count", "aliases_json", "nicknames_json", "tags_json",
	"social_links_json", "awards_json", "timeline_json", "public_roles_json",
	"data_conflicts_json", "created_at", "updated_at",
}

// writeOfflineCSV 按真实导出文件的形态写一份 CSV（含 UTF-8 BOM）。
func writeOfflineCSV(t *testing.T, path string, rows []map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF}) // 真实导出文件带 BOM
	w := csv.NewWriter(&buf)
	if err := w.Write(offlineCSVHeader); err != nil {
		t.Fatalf("写表头: %v", err)
	}
	for i, row := range rows {
		rec := make([]string, len(offlineCSVHeader))
		for j, c := range offlineCSVHeader {
			rec[j] = row[c]
		}
		if err := w.Write(rec); err != nil {
			t.Fatalf("写第 %d 行: %v", i+1, err)
		}
	}
	w.Flush()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
}

func TestOfflineLibraryFetch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.csv")
	writeOfflineCSV(t, p, []map[string]string{
		{
			"id": "10183", "name_original": "坂井なな", "name_ja": "坂井なな",
			"name_romanized": "Nana Sakai", "kana": "さかいなな",
			"birthdate": "1991/04/19", "birthplace": "東京都", "blood_type": "A",
			"height_cm": "163.0", "bust_cm": "88.0", "waist_cm": "59.0", "hip_cm": "85.5",
			"cup": "E", "agency": "T♡Project", "hobbies": "ゲーム",
			"debut_date": "2013-03-09", "retirement_date": "2018-12-01",
			"biography_zh_cn": "简介正文", "official_site": "https://example.invalid/nana",
			"aliases_json": `["本多翼、白瀬真希","本多翼"]`,
		},
		{"id": "10184", "name_original": "空条目", "name_ja": "空条目"},
	})
	s := newOfflineLibrarySource(p)
	ctx := context.Background()

	// 本名命中：分数 100
	f, err := s.Fetch(ctx, nil, "坂井なな", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f == nil {
		t.Fatal("本名命中应该返回资料")
	}
	if f.MatchScore != 100 {
		t.Errorf("本名命中的置信度应为 100，实际 %d", f.MatchScore)
	}
	if f.Source != offlineLibSourceKey {
		t.Errorf("Source 应为 %q，实际 %q", offlineLibSourceKey, f.Source)
	}
	// BOM 没剥掉的话列名全对不上，这里就会是空 —— 这条断言实际上在守 BOM。
	if f.Summary != "简介正文" {
		t.Errorf("简介没带出来（列名对不上？BOM 没剥？）：%q", f.Summary)
	}
	if f.BirthPlace != "東京都" || f.Agency != "T♡Project" {
		t.Errorf("出生地/事务所没带出来：%q / %q", f.BirthPlace, f.Agency)
	}
	// 别的工具的写法不受我们控制，日期要收敛成 Emby 认的 YYYY-MM-DD
	if f.BirthDate != "1991-04-19" {
		t.Errorf("日期应收敛成 YYYY-MM-DD，实际 %q", f.BirthDate)
	}
	if f.DebutDate != "2013-03-09" || f.RetirementDate != "2018-12-01" {
		t.Errorf("出道/退役日期没带出来：%q / %q", f.DebutDate, f.RetirementDate)
	}
	// 导出脚本把数值列写成了浮点，`163.0` 直接进简介会变成「身高：163.0 cm」，
	// 和别的源（`163`）摆在一起就是两种格式。
	if f.Height != "163" || f.Hip != "85.5" {
		t.Errorf("数值列应当去掉多余的 .0 且保留有效小数：height=%q hip=%q", f.Height, f.Hip)
	}
	if f.SourceURL != "https://example.invalid/nana" {
		t.Errorf("来源链接没带出来：%q", f.SourceURL)
	}

	// 别名命中：分数 95（与 nameMatchScore 里「命中别名记 95」一致，不另设一套）
	f2, err := s.Fetch(ctx, nil, "别人", []string{"さかいなな"})
	if err != nil {
		t.Fatalf("Fetch(别名): %v", err)
	}
	if f2 == nil || f2.MatchScore != offlineLibAliasScore {
		t.Errorf("别名命中应返回置信度 %d 的资料，实际 %+v", offlineLibAliasScore, f2)
	}
	// 罗马音与假名也要能查到：它们同样是「这个人的写法」，
	// 而别的资料源（avdatabank 等）恰恰是靠罗马音检索的。
	for _, k := range []string{"Nana Sakai", "さかいなな"} {
		if f, err := s.Fetch(ctx, nil, k, nil); err != nil || f == nil {
			t.Errorf("%q 应当能查到人：f=%+v err=%v", k, f, err)
		}
	}
	// `["本多翼、白瀬真希","本多翼"]` 里第一条塞了两个名字，得拆开才能各自进索引 ——
	// 不拆的话「白瀬真希」永远查不到，而库里其实有。
	if f, err := s.Fetch(ctx, nil, "白瀬真希", nil); err != nil || f == nil {
		t.Errorf("顿号分隔的别名应当被拆开并进索引：f=%+v err=%v", f, err)
	}

	// 查无此人：是「未命中」，不是错误（和别的源一样，不该在面板上冒告警）
	f3, err := s.Fetch(ctx, nil, "查无此人", []string{"也没有"})
	if err != nil {
		t.Errorf("未命中不该报错：%v", err)
	}
	if f3 != nil {
		t.Errorf("未命中应返回 nil，实际 %+v", f3)
	}

	// 整条空的条目（连别名都没有）不许报成命中：否则「命中来源」里会多一个
	// 什么都没提供的源，用户还得猜它为什么在那儿。
	f4, err := s.Fetch(ctx, nil, "空条目", nil)
	if err != nil {
		t.Fatalf("Fetch(空条目): %v", err)
	}
	if f4 != nil {
		t.Errorf("空条目不应当命中，实际 %+v", f4)
	}
}

// TestOfflineLibraryReportsAmbiguousName 一个写法对应多条记录时必须说出来。
//
// 实测这份库里 `name_original` 有 15% 的键同时属于两条以上记录。悄悄挑一条的
// 后果是把别人的出生日期写进用户的 Emby，而界面上看起来毫无异常。
func TestOfflineLibraryReportsAmbiguousName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.csv")
	writeOfflineCSV(t, p, []map[string]string{
		{"id": "10188", "name_original": "AIKA", "name_ja": "AIKA", "birthdate": "1990-08-25", "height_cm": "163"},
		{"id": "23848", "name_original": "AIKA", "name_ja": "AIKA", "birthdate": "1975-11-02", "height_cm": "155"},
	})
	s := newOfflineLibrarySource(p)

	f, err := s.Fetch(context.Background(), nil, "AIKA", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f == nil {
		t.Fatal("重名也应当给出资料（只是要说明情况）")
	}
	if f.Note == "" {
		t.Fatal("一个写法对应多条记录时必须留下 Note —— 否则用户以为库里就是这么写的")
	}
	if !strings.Contains(f.Note, "23848") {
		t.Errorf("Note 要点出另一条记录的 id，实际：%q", f.Note)
	}
	// 认下的那一条按文件顺序取先出现的 —— 这是**确定的**行为，不是随机。
	if f.BirthDate != "1990-08-25" {
		t.Errorf("多条同写法时取先出现的那条，实际 %q", f.BirthDate)
	}

	// 不重名的写法不该平白多一条告警
	writeOfflineCSV(t, p, []map[string]string{
		{"id": "1", "name_original": "独一无二", "name_ja": "独一无二", "birthdate": "1990-08-25"},
	})
	time.Sleep(20 * time.Millisecond)
	f3, err := s.Fetch(context.Background(), nil, "独一无二", nil)
	if err != nil || f3 == nil {
		t.Fatalf("Fetch: %v %+v", err, f3)
	}
	if f3.Note != "" {
		t.Errorf("不重名的写法不该有 Note，实际：%q", f3.Note)
	}
}

// TestOfflineLibraryMissingFileReportsWhy 读不到的时候必须**报错**而不是静默未命中。
//
// 静默未命中的后果：用户开了开关、看着一切正常，只是这个人「碰巧没资料」——
// 一百个人都这样也看不出是路径写错了。
func TestOfflineLibraryMissingFileReportsWhy(t *testing.T) {
	s := newOfflineLibrarySource(filepath.Join(t.TempDir(), "nope.csv"))
	_, err := s.Fetch(context.Background(), nil, "谁", nil)
	if err == nil {
		t.Fatal("导出文件不存在时必须报错，不能静默当成未命中")
	}
	if !strings.Contains(err.Error(), "sqlcipher_dump") {
		t.Errorf("错误信息要告诉用户这个文件是怎么生成的，实际：%v", err)
	}

	// 同一句话还得能从 Stats() 拿到 —— 那条路径给的是**设置页**那行状态。
	// 第一版这里各写各的：Stats() 只回「读不到导出文件：<系统错误>」，
	// 于是最该看到的「先用脚本导出」只在抽屉的告警里出现，
	// 用户在设置页盯着一个「读不到」完全无从下手。
	_, errMsg := s.Stats()
	if !strings.Contains(errMsg, "sqlcipher_dump") {
		t.Errorf("设置页状态行也要带上「怎么生成这个文件」，实际：%q", errMsg)
	}
	if errMsg != err.Error() {
		t.Errorf("两处文案必须一致：Stats()=%q / Fetch()=%q", errMsg, err.Error())
	}
}

// TestOfflineLibraryRejectsWrongFile 拿错了文件要明确说「这不是那种文件」。
//
// 用户手上很容易有个同名的 CSV（比如从别处导的），静默当成「库里没这个人」
// 会让人一直以为库是空的。所以这里要的是**报错**，而且报错里要有下一步。
func TestOfflineLibraryRejectsWrongFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "other.csv")
	if err := os.WriteFile(p, []byte("a,b,c\n1,2,3\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	s := newOfflineLibrarySource(p)

	_, err := s.Fetch(context.Background(), nil, "谁", nil)
	if err == nil {
		t.Fatal("不是资料库导出的 CSV 时必须报错")
	}
	if !strings.Contains(err.Error(), "name_original") {
		t.Errorf("错误信息要指出缺的是哪一列，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "sqlcipher_dump") {
		t.Errorf("错误信息要带上「怎么生成这个文件」，实际：%v", err)
	}
}

// TestOfflineLibraryAcceptsFileWithoutBOM 没有 BOM 的导出文件也得能读。
//
// BOM 的坑是「有 BOM 时列名对不上」，所以两种都要覆盖：只测带 BOM 的话，
// 哪天为了修 BOM 把剥离逻辑写成无条件截 3 字节，就会静默吃掉真实数据的头 3 字节。
func TestOfflineLibraryAcceptsFileWithoutBOM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plain.csv")
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(offlineCSVHeader); err != nil {
		t.Fatalf("写表头: %v", err)
	}
	rec := make([]string, len(offlineCSVHeader))
	for i, c := range offlineCSVHeader {
		switch c {
		case "id":
			rec[i] = "7"
		case "name_original":
			rec[i] = "无BOM"
		case "biography_zh_cn":
			rec[i] = "有内容才算命中"
		}
	}
	if err := w.Write(rec); err != nil {
		t.Fatalf("写行: %v", err)
	}
	w.Flush()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}

	s := newOfflineLibrarySource(p)
	f, err := s.Fetch(context.Background(), nil, "无BOM", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f == nil || f.MatchedName != "无BOM" {
		t.Errorf("无 BOM 的文件也要能读，实际 %+v", f)
	}
}

// TestOfflineLibraryReloadsWhenFileChanges 重新导出之后要能生效。
//
// 索引是懒加载的，按 mtime + size 失效 —— 读一次就再也不看的话，
// 用户重新导出一份新库会毫无反应，而且这种「不生效」极难自查。
func TestOfflineLibraryReloadsWhenFileChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.csv")
	writeOfflineCSV(t, p, []map[string]string{{"id": "1", "name_original": "甲", "name_ja": "甲", "biography_zh_cn": "一"}})
	s := newOfflineLibrarySource(p)
	ctx := context.Background()

	if f, _ := s.Fetch(ctx, nil, "甲", nil); f == nil {
		t.Fatal("先要能查到甲")
	}
	if f, _ := s.Fetch(ctx, nil, "乙", nil); f != nil {
		t.Fatal("这时候还查不到乙")
	}

	time.Sleep(20 * time.Millisecond) // 保证 mtime 真的变了
	writeOfflineCSV(t, p, []map[string]string{
		{"id": "1", "name_original": "甲", "name_ja": "甲", "biography_zh_cn": "一"},
		{"id": "2", "name_original": "乙", "name_ja": "乙", "biography_zh_cn": "二"},
	})

	f, err := s.Fetch(ctx, nil, "乙", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f == nil {
		t.Error("重新导出之后应该能查到新增的条目（索引要按 mtime/size 失效）")
	}
}

// TestOfflineLibraryIsFirstPriorityForProfiles 钉住需求里的「第一优先级」。
func TestOfflineLibraryIsFirstPriorityForProfiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.csv")
	writeOfflineCSV(t, p, []map[string]string{{"id": "1", "name_original": "甲", "name_ja": "甲", "biography_zh_cn": "离线简介"}})
	a := testApp(t, "", "")
	if err := a.store.Update(func(c *Config) {
		c.OfflineDBEnabled = true
		c.OfflineDBPath = p
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	srcs := a.profileSourceList()
	if len(srcs) == 0 {
		t.Fatal("资料源清单不该为空")
	}
	if srcs[0].Key() != offlineLibSourceKey {
		keys := make([]string, 0, len(srcs))
		for _, s := range srcs {
			keys = append(keys, s.Key())
		}
		t.Fatalf("启用离线库后它必须排在最前面（mergeFacts 是先到先得），实际顺序 %v", keys)
	}

	// 关掉：不该再出现在清单里
	if err := a.store.Update(func(c *Config) { c.OfflineDBEnabled = false }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, s := range a.profileSourceList() {
		if s.Key() == offlineLibSourceKey {
			t.Error("关掉之后离线库不该还留在资料源清单里")
		}
	}

	// 只填路径、没开开关也一样不启用（两个条件缺一不可）
	if err := a.store.Update(func(c *Config) { c.OfflineDBPath = p }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if a.offlineLib() != nil {
		t.Error("开关没打开时不该启用离线库")
	}
}

// TestOfflineLibrarySharesAliasMemory 钉住「与别名记忆联动」。
//
// 这条链路是：Emby 里的人名 →（别名记忆给出已确认的旧艺名）→ 每个资料源都拿到
// 这些候选写法 → 离线库按旧艺名命中。也就是「用户确认过一次的写法，之后不用再手输」。
//
// 反证同样重要：**关掉别名记忆就该查不到**，否则这个测试证明不了是别名记忆起的作用。
func TestOfflineLibrarySharesAliasMemory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.csv")
	// 库里只有旧艺名「本多翼」，没有 Emby 里的那个名字。
	writeOfflineCSV(t, p, []map[string]string{{
		"id": "10183", "name_original": "本多翼", "name_ja": "本多翼",
		"birthdate": "1990-11-12", "height_cm": "165",
		"aliases_json": `["白瀬真希"]`,
	}})

	m := newMockEmby(t)
	a := newProfileTestApp(t, m)
	if err := a.store.Update(func(c *Config) {
		c.OfflineDBEnabled = true
		c.OfflineDBPath = p
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	ctx := context.Background()
	// 只走离线源：另外三个源要联网，单测里不碰。
	only := FetchOptions{Sources: []string{offlineLibSourceKey}}

	// 1) 别名记忆是空的：Emby 里的名字在库里查不到。
	off := only
	off.UseAliasMemo = false
	prof, err := a.fetchActorProfileWith(ctx, a.profileSourceList(), "", "水野朝陽", "", off)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(prof.Facts) != 0 {
		t.Fatalf("还没记过别名时不该命中，实际 %d 条", len(prof.Facts))
	}

	// 2) 把「本多翼」记进别名记忆（等价于上次采用/同步时确认过）。
	a.aliases.Remember("水野朝陽", []string{"本多翼"}, "测试")

	on := only
	on.UseAliasMemo = true
	prof, err = a.fetchActorProfileWith(ctx, a.profileSourceList(), "", "水野朝陽", "", on)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(prof.Facts) != 1 {
		t.Fatalf("别名记忆里的旧艺名应当让离线库命中，实际 %d 条", len(prof.Facts))
	}
	if prof.Facts[0].MatchScore != offlineLibAliasScore {
		t.Errorf("按别名命中记 %d 分，实际 %d", offlineLibAliasScore, prof.Facts[0].MatchScore)
	}
	if prof.Facts[0].BirthDate != "1990-11-12" {
		t.Errorf("命中的应当是库里那条记录，实际出生日期 %q", prof.Facts[0].BirthDate)
	}
	// 库里带的其他写法也要一并暴露出去 —— 它们是下一轮的记忆来源。
	if !slices.Contains(prof.Aliases, "白瀬真希") {
		t.Errorf("库里的别名应当出现在结果里，实际 %v", prof.Aliases)
	}
}

// TestActorFactsHasNoImageField 把「离线库不提供头像」这条需求钉在类型上。
//
// 需求原文：「头像仍使用独立头像源顺序，不会默认使用离线库头像」。
// 靠代码评审去记住这件事不牢靠 —— 只要哪天有人往 ActorFacts 里加一个图片字段，
// 这条链路（它同样返回 ActorFacts）就有机会把头像一并供出来。所以这里用反射守着：
// 图片字段一旦出现，这条先红，再去讨论「离线库要不要填它」。
//
// 另外：导出文件里明明有一列 `profile_image_url`（25,234 条非空），本文件的有意
// 不映射它也是这条需求的一部分。
func TestActorFactsHasNoImageField(t *testing.T) {
	ty := reflect.TypeOf(ActorFacts{})
	for i := 0; i < ty.NumField(); i++ {
		name := strings.ToLower(ty.Field(i).Name)
		for _, bad := range []string{"image", "avatar", "poster", "photo", "thumb", "picture"} {
			if strings.Contains(name, bad) {
				t.Errorf("ActorFacts.%s 看起来是图片字段。资料源（含离线库）负责的是文本资料，"+
					"头像必须留在独立的头像源链路上", ty.Field(i).Name)
			}
		}
	}
}

// TestOfflineLibraryDoesNotMapProfileImage 导出文件里的头像 URL 必须**不**被映射进来。
func TestOfflineLibraryDoesNotMapProfileImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.csv")
	writeOfflineCSV(t, p, []map[string]string{{
		"id": "1", "name_original": "甲", "name_ja": "甲",
		"profile_image_url": "https://laoshi.ink/assets/img/celebrities/jav/甲.jpg",
		"biography_zh_cn":   "有简介",
	}})
	s := newOfflineLibrarySource(p)
	f, err := s.Fetch(context.Background(), nil, "甲", nil)
	if err != nil || f == nil {
		t.Fatalf("Fetch: %v %+v", err, f)
	}
	// 类型上没有图片字段可放，这条其实是「有人加了字段就会红」之外的第二道保险：
	// 断言这个 URL 没有以任何形式混进返回结果（比如被塞进 SourceURL）。
	for _, v := range []string{f.SourceURL, f.Summary, f.BirthPlace} {
		if strings.Contains(v, "laoshi.ink") {
			t.Errorf("导出文件里的头像 URL (%s) 不该出现在资料字段里", v)
		}
	}
}

// TestSaveConfigKeepsAbsentKeys 「缺键 ≠ 清空」这条规则的回归。
//
// /api/config 被两个地方调用：设置页发完整配置，登录页的高级配置只发其中一部分。
// 布尔项反序列化后没法区分「没提交」和「提交了 false」，直接赋值就等于
// 「用登录页那份配置把用户的开关全关掉」—— 而且完全静默。
func TestSaveConfigKeepsAbsentKeys(t *testing.T) {
	a := testApp(t, "", "")
	if err := a.store.Update(func(c *Config) {
		c.AutoRefresh = true
		c.OverwriteImages = true
		c.OfflineDBEnabled = true
		c.OfflineDBPath = `F:\x\actresses_export.csv`
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
		w := httptest.NewRecorder()
		a.handleSaveConfig(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d：%s", w.Code, w.Body.String())
		}
	}

	// 模拟登录页的高级配置：只带一部分键，**完全不含**这三个开关
	post(`{"emby_url":"http://e:8096","proxy":"","insecure_tls":true}`)
	c := a.store.Get()
	if !c.AutoRefresh {
		t.Error("auto_refresh 没在这个请求里出现，不该被关掉")
	}
	if !c.OverwriteImages {
		t.Error("overwrite_images 没在这个请求里出现，不该被关掉")
	}
	if !c.OfflineDBEnabled || c.OfflineDBPath == "" {
		t.Errorf("离线库配置没在这个请求里出现，不该被清掉：enabled=%v path=%q",
			c.OfflineDBEnabled, c.OfflineDBPath)
	}

	// 反过来：显式提交 false 必须真的关掉 —— 否则「缺键不覆盖」就成了「永远关不掉」
	post(`{"offline_db_enabled":false}`)
	if a.store.Get().OfflineDBEnabled {
		t.Error("显式提交 offline_db_enabled:false 应该真的关掉")
	}
	// 关掉之后路径要留着：下次打开还认得那个文件
	if a.store.Get().OfflineDBPath == "" {
		t.Error("关掉开关不该顺手把路径清掉")
	}
}
