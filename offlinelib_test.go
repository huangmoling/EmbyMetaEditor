package main

// 离线演员资料库的回归测试。
//
// 这个源的数据来自「另一个工具的加密资料库」→ 由 tools/export_offline_db.py
// 导出成 JSON。所以夹具就是一份导出 JSON（结构见 offlineLibFile），
// 不联网、不碰那个 .db。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeOfflineLibrary(t *testing.T, dir string, entries []offlineEntry) string {
	t.Helper()
	p := filepath.Join(dir, "offline_library.json")
	b, err := json.Marshal(offlineLibFile{Version: 1, Source: "离线演员资料库", Entries: entries})
	if err != nil {
		t.Fatalf("构造导出夹具: %v", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("写导出夹具: %v", err)
	}
	return p
}

func TestOfflineLibraryFetch(t *testing.T) {
	dir := t.TempDir()
	p := writeOfflineLibrary(t, dir, []offlineEntry{
		{Name: "坂井なな", Aliases: []string{"さかいなな"}, Summary: "简介正文", BirthDate: "1991/04/19"},
		{Name: "空条目"},
	})
	s := newOfflineLibrarySource(p)
	ctx := context.Background()

	// 本名命中：分数 100，来源标记要能写进 ProviderIds
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
	if f.Summary != "简介正文" {
		t.Errorf("简介没带出来：%q", f.Summary)
	}
	// 别的工具的写法不受我们控制，日期要收敛成 Emby 认的 YYYY-MM-DD
	if f.BirthDate != "1991-04-19" {
		t.Errorf("日期应收敛成 YYYY-MM-DD，实际 %q", f.BirthDate)
	}

	// 别名命中：分数 95（与 nameMatchScore 里「命中别名记 95」一致，不另设一套）
	f2, err := s.Fetch(ctx, nil, "别人", []string{"さかいなな"})
	if err != nil {
		t.Fatalf("Fetch(别名): %v", err)
	}
	if f2 == nil || f2.MatchScore != offlineLibAliasScore {
		t.Errorf("别名命中应返回置信度 %d 的资料，实际 %+v", offlineLibAliasScore, f2)
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

// TestOfflineLibraryMissingFileReportsWhy 读不到的时候必须**报错**而不是静默未命中。
//
// 静默未命中的后果：用户开了开关、看着一切正常，只是这个人「碰巧没资料」——
// 一百个人都这样也看不出是路径写错了。
func TestOfflineLibraryMissingFileReportsWhy(t *testing.T) {
	s := newOfflineLibrarySource(filepath.Join(t.TempDir(), "nope.json"))
	_, err := s.Fetch(context.Background(), nil, "谁", nil)
	if err == nil {
		t.Fatal("导出文件不存在时必须报错，不能静默当成未命中")
	}
	if !strings.Contains(err.Error(), "export_offline_db") {
		t.Errorf("错误信息要告诉用户这个文件是怎么生成的，实际：%v", err)
	}

	// 同一句话还得能从 Stats() 拿到 —— 那条路径给的是**设置页**那行状态。
	// 第一版这里各写各的：Stats() 只回「读不到导出文件：<系统错误>」，
	// 于是最该看到的「先用 export_offline_db.py 导出」只在抽屉的告警里出现，
	// 用户在设置页盯着一个「读不到」完全无从下手。
	_, errMsg := s.Stats()
	if !strings.Contains(errMsg, "export_offline_db") {
		t.Errorf("设置页状态行也要带上「怎么生成这个文件」，实际：%q", errMsg)
	}
	if errMsg != err.Error() {
		t.Errorf("两处文案必须一致：Stats()=%q / Fetch()=%q", errMsg, err.Error())
	}
}

// TestOfflineLibraryReloadsWhenFileChanges 重新导出之后要能生效。
//
// 索引是懒加载的，按 mtime + size 失效 —— 读一次就再也不看的话，
// 用户重新导出一份新库会毫无反应，而且这种「不生效」极难自查。
func TestOfflineLibraryReloadsWhenFileChanges(t *testing.T) {
	dir := t.TempDir()
	p := writeOfflineLibrary(t, dir, []offlineEntry{{Name: "甲", Summary: "一"}})
	s := newOfflineLibrarySource(p)
	ctx := context.Background()

	if f, _ := s.Fetch(ctx, nil, "甲", nil); f == nil {
		t.Fatal("先要能查到甲")
	}
	if f, _ := s.Fetch(ctx, nil, "乙", nil); f != nil {
		t.Fatal("这时候还查不到乙")
	}

	time.Sleep(20 * time.Millisecond) // 保证 mtime 真的变了
	writeOfflineLibrary(t, dir, []offlineEntry{
		{Name: "甲", Summary: "一"}, {Name: "乙", Summary: "二"},
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
	p := writeOfflineLibrary(t, dir, []offlineEntry{{Name: "甲", Summary: "离线简介"}})
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

// TestActorFactsHasNoImageField 把「离线库不提供头像」这条需求钉在类型上。
//
// 需求原文：「头像仍使用独立头像源顺序，不会默认使用离线库头像」。
// 靠代码评审去记住这件事不牢靠 —— 只要哪天有人往 ActorFacts 里加一个图片字段，
// 这条链路（它同样返回 ActorFacts）就有机会把头像一并供出来。所以这里用反射守着：
// 图片字段一旦出现，这条先红，再去讨论「离线库要不要填它」。
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
		c.OfflineDBPath = `F:\x\offline_library.json`
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
