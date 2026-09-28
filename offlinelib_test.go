package main

// 离线演员资料库的回归测试。
//
// 这个源的数据来自「另一个工具的加密资料库」→ 由 tools/sqlcipher_dump.py 导出成
// CSV → 放在 data/actresses_export.csv → 用 //go:embed 编译进程序。所以这里不碰
// 文件系统、不碰那个 .db：夹具是**内存里的 CSV 字节**，真实数据只做「有没有内嵌进来、
// 解析得出来吗」这类整体校验。
//
// 夹具**必须走真实的列名与 41 列表头**，而不是直接构造 offlineEntry：
// 这条链路上最容易出错的地方就是「列名对不上」（真实的坑：文件带 UTF-8 BOM，
// 第一列会变成 "\ufeffid"，于是所有字段都读成空，而文件看起来完全正常），
// 那种错在直接构造结构体的测试里永远暴露不出来。

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
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

// offlineCSVBytes 按真实导出文件的形态拼一份 CSV 字节（默认带 UTF-8 BOM）。
func offlineCSVBytes(rows []map[string]string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF}) // 真实导出文件带 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write(offlineCSVHeader)
	for _, row := range rows {
		rec := make([]string, len(offlineCSVHeader))
		for j, c := range offlineCSVHeader {
			rec[j] = row[c]
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes()
}

// bytesFor 是「写夹具」的统一入口：一组记录 → 一个可直接喂给源的字节切片。
func bytesFor(rows ...map[string]string) []byte { return offlineCSVBytes(rows) }

func TestOfflineLibraryFetch(t *testing.T) {
	s := newOfflineLibraryFromBytes(bytesFor(
		map[string]string{
			"id": "10183", "name_original": "坂井なな", "name_ja": "坂井なな",
			"name_romanized": "Nana Sakai", "kana": "さかいなな",
			"birthdate": "1991/04/19", "birthplace": "東京都", "blood_type": "A",
			"height_cm": "163.0", "bust_cm": "88.0", "waist_cm": "59.0", "hip_cm": "85.5",
			"cup": "E", "agency": "T♡Project", "hobbies": "ゲーム",
			"debut_date": "2013-03-09", "retirement_date": "2018-12-01",
			"biography_zh_cn": "简介正文", "official_site": "https://example.invalid/nana",
			"aliases_json": `["本多翼、白瀬真希","本多翼"]`,
		},
		map[string]string{"id": "10184", "name_original": "空条目", "name_ja": "空条目"},
	))
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

// TestOfflineLibraryParsesBuiltinData 内嵌数据必须真的解析得出来。
//
// 「数据编译进程序」这类设计最典型的坏法不是崩溃，而是**界面一切正常、只是永远
// 查不到人**：//go:embed 指向的文件被 gitignore 掉了、换行被 checkout 改掉了、
// 换了机器忘了带上 data/ —— 全都表现为「解析出来 0 条」或者「查谁都查不到」。
// 所以这里既看条数，也真的拿数据里第一条去查一次。
func TestOfflineLibraryParsesBuiltinData(t *testing.T) {
	if len(builtinOfflineCSV) == 0 {
		t.Fatal("内嵌数据是空的 —— //go:embed data/actresses_export.csv 没生效（文件没入库？）")
	}
	s := newOfflineLibrary()
	n, errMsg := s.Stats()
	if errMsg != "" {
		t.Fatalf("内嵌数据解析失败：%s", errMsg)
	}
	// 不写死 27780：重新导出之后条数会变，那是正常的。但**数量级**掉了就说明
	// 内嵌的那份不是完整的导出，必须有人看一眼。
	if n < 27000 {
		t.Errorf("内嵌数据的条目数偏少：%d（重新导出了？还是入库的是一份残缺文件？）", n)
	}
	// 索引键数必然多于记录数：一个人有本名/日文名/罗马音/假名/别名好几个写法。
	if s.KeyCount() <= n {
		t.Errorf("索引键数应当多于记录数：keys=%d names=%d", s.KeyCount(), n)
	}

	entries, err := parseOfflineCSV(builtinOfflineCSV)
	if err != nil {
		t.Fatalf("parseOfflineCSV: %v", err)
	}
	// 挑第一条**除了名字之外还有内容**的记录去查（空壳记录按设计就是查不到的）。
	var probe *offlineEntry
	for i := range entries {
		if entries[i].hasContent() {
			probe = &entries[i]
			break
		}
	}
	if probe == nil {
		t.Fatal("整个库里没有一条带内容的记录，数据不像是对的那份")
	}
	f, err := s.Fetch(context.Background(), nil, probe.name(), nil)
	if err != nil {
		t.Fatalf("Fetch(%q): %v", probe.name(), err)
	}
	if f == nil {
		t.Fatalf("内嵌数据里存在的写法 %q 应当查得到", probe.name())
	}
	if f.MatchedName == "" {
		t.Error("命中要带出本名")
	}
}

// TestOfflineLibraryReportsAmbiguousName 一个写法对应多条记录时必须说出来。
//
// 实测这份库里 `name_original` 有 15% 的键同时属于两条以上记录。悄悄挑一条的
// 后果是把别人的出生日期写进用户的 Emby，而界面上看起来毫无异常。
func TestOfflineLibraryReportsAmbiguousName(t *testing.T) {
	s := newOfflineLibraryFromBytes(bytesFor(
		map[string]string{"id": "10188", "name_original": "AIKA", "name_ja": "AIKA", "birthdate": "1990-08-25", "height_cm": "163"},
		map[string]string{"id": "23848", "name_original": "AIKA", "name_ja": "AIKA", "birthdate": "1975-11-02", "height_cm": "155"},
	))

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
}

// TestOfflineLibraryBadDataReportsWhy 认不出的数据必须**报错**，不能静默当成「库里没这个人」。
//
// 静默未命中的后果：一切看着正常，只是这个人「碰巧没资料」—— 一百个人都这样也看不出
// 是数据坏在哪。另外这句话里的出处（sqlcipher_dump / data/ 那份 CSV）必须留着：
// 内嵌数据解析失败只可能是**构建产物**有问题，指向「怎么生成这份数据」才有意义。
func TestOfflineLibraryBadDataReportsWhy(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string // 错误里必须出现的关键词
	}{
		{"拿错了别的 CSV", []byte("a,b,c\n1,2,3\n"), "name_original"},
		{"只有表头没有数据行", offlineCSVBytes(nil), "一个有效条目都没有"},
		{"完全是空的", nil, "读表头失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newOfflineLibraryFromBytes(c.raw)
			_, err := s.Fetch(context.Background(), nil, "谁", nil)
			if err == nil {
				t.Fatal("数据认不出来时必须报错，不能静默当成未命中")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息里应当说明问题（期望含 %q），实际：%v", c.want, err)
			}
			if !strings.Contains(err.Error(), "sqlcipher_dump") {
				t.Errorf("错误信息要指出这份数据是怎么生成的，实际：%v", err)
			}

			// 同一句话还得能从 Stats() 拿到 —— 诊断路径（以及单测自己）靠它确认
			// 「内嵌的这份数据到底能不能用」。第一版这里各写各的，于是最该看到的那句
			// 只在抓取告警里出现，另一个出口只回光秃秃的系统错误。
			_, errMsg := s.Stats()
			if !strings.Contains(errMsg, "sqlcipher_dump") {
				t.Errorf("Stats() 也要带上「数据是怎么来的」，实际：%q", errMsg)
			}
			if errMsg != err.Error() {
				t.Errorf("两处文案必须一致：Stats()=%q / Fetch()=%q", errMsg, err.Error())
			}
		})
	}
}

// TestOfflineLibraryAcceptsDataWithoutBOM 没有 BOM 的导出文件也得能读。
//
// BOM 的坑是「有 BOM 时列名对不上」，所以两种都要覆盖：只测带 BOM 的话，
// 哪天为了修 BOM 把剥离逻辑写成无条件截 3 字节，就会静默吃掉真实数据的头 3 字节。
func TestOfflineLibraryAcceptsDataWithoutBOM(t *testing.T) {
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

	s := newOfflineLibraryFromBytes(buf.Bytes())
	f, err := s.Fetch(context.Background(), nil, "无BOM", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f == nil || f.MatchedName != "无BOM" {
		t.Errorf("无 BOM 的数据也要能读，实际 %+v", f)
	}
}

// TestOfflineLibraryIsFirstPriorityForProfiles 钉住需求里的「第一优先级」。
//
// v1.10.0 起这个源**不可关、不可缺席**：数据编译进程序，没有配置项，
// 所以它在清单里的位置是个常量，而不是「开关打开时才成立」。
func TestOfflineLibraryIsFirstPriorityForProfiles(t *testing.T) {
	a := testApp(t, "", "")

	srcs := a.profileSourceList()
	if len(srcs) == 0 {
		t.Fatal("资料源清单不该为空")
	}
	if srcs[0].Key() != offlineLibSourceKey {
		keys := make([]string, 0, len(srcs))
		for _, s := range srcs {
			keys = append(keys, s.Key())
		}
		t.Fatalf("离线库必须排在最前面（mergeFacts 是先到先得），实际顺序 %v", keys)
	}
	// 它不能因为「配置里没提到它」就消失 —— 这正是「内嵌」与「可配置」的分界。
	if lib := a.offlineLib(); lib == nil || lib.Key() != offlineLibSourceKey {
		t.Errorf("离线库必须永远可用，实际 %+v", lib)
	}
}

// TestOfflineLibrarySharesAliasMemory 钉住「与别名记忆联动」。
//
// 这条链路是：Emby 里的人名 →（别名记忆给出已确认的旧艺名）→ 每个资料源都拿到
// 这些候选写法 → 离线库按旧艺名命中。也就是「用户确认过一次的写法，之后不用再手输」。
//
// 反证同样重要：**关掉别名记忆就该查不到**，否则这个测试证明不了是别名记忆起的作用。
func TestOfflineLibrarySharesAliasMemory(t *testing.T) {
	m := newMockEmby(t)
	a := newProfileTestApp(t, m)
	// 库里只有旧艺名「本多翼」，没有 Emby 里的那个名字。
	a.offlineSrc = newOfflineLibraryFromBytes(bytesFor(map[string]string{
		"id": "10183", "name_original": "本多翼", "name_ja": "本多翼",
		"birthdate": "1990-11-12", "height_cm": "165",
		"aliases_json": `["白瀬真希"]`,
	}))

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
	s := newOfflineLibraryFromBytes(bytesFor(map[string]string{
		"id": "1", "name_original": "甲", "name_ja": "甲",
		"profile_image_url": "https://laoshi.ink/assets/img/celebrities/jav/甲.jpg",
		"biography_zh_cn":   "有简介",
	}))
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

	// 模拟登录页的高级配置：只带一部分键，**完全不含**这两个开关
	post(`{"emby_url":"http://e:8096","proxy":"","insecure_tls":true}`)
	c := a.store.Get()
	if !c.AutoRefresh {
		t.Error("auto_refresh 没在这个请求里出现，不该被关掉")
	}
	if !c.OverwriteImages {
		t.Error("overwrite_images 没在这个请求里出现，不该被关掉")
	}

	// 反过来：显式提交 false 必须真的关掉 —— 否则「缺键不覆盖」就成了「永远关不掉」
	post(`{"auto_refresh":false}`)
	if a.store.Get().AutoRefresh {
		t.Error("显式提交 auto_refresh:false 应该真的关掉")
	}
}
