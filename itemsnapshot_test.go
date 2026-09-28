package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// formatSame 判断两个值在**用户眼里**是不是同一个值。
//
// 不能直接用 DeepEqual：快照里的数字是 JSON 解出来的 float64，而回滚补的
// 空值可能是 int 0；比较展示形态才和界面看到的一致（这也是 previewPatch 的口径）。
func formatSame(a, b any) bool { return formatFieldValue(a) == formatFieldValue(b) }

// isBlank 判断一个值算不算「空」（回滚到「原来没有」时的目标形态）。
func isBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case []string:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case float64:
		return t == 0
	case int:
		return t == 0
	default:
		return isNilVal(v)
	}
}

// 刮削必须**先留快照再写**，而且回滚要能把元数据还原回去。
// 这是 Emby 写入不可逆这个前提下唯一的后悔药。
func TestScrapeMovieLeavesRollbackSnapshot(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)

	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001 某个片名", "Type": "Movie",
		"Path":     "F:\\媒体\\SSIS-001\\SSIS-001.strm",
		"Overview": "原有简介不能丢", "Tags": []any{"旧标签"},
		"ProviderIds": map[string]any{}, "ImageTags": map[string]any{},
	}

	res, err := app.ScrapeMovie(context.Background(), "m1", ScrapeOptions{})
	if err != nil {
		t.Fatalf("刮削失败: %v", err)
	}
	if res.SnapshotID == "" {
		t.Fatal("刮削没有留下快照 —— 写错了就没法回滚了")
	}

	rec, ok := app.sync.Get(res.SnapshotID)
	if !ok {
		t.Fatal("快照 ID 换不到记录")
	}
	if rec.kindOrDefault() != syncKindItem {
		t.Errorf("Kind = %q，期望 item", rec.kindOrDefault())
	}
	if rec.ItemID != "m1" {
		t.Errorf("ItemID = %q，期望 m1", rec.ItemID)
	}
	if len(rec.Changed) == 0 || len(rec.After) == 0 {
		t.Fatalf("快照里没有字段：changed=%v after=%v", rec.Changed, rec.After)
	}
	// 写前的值必须是**旧的**（顺序反了就变成新值，回滚等于没回）
	if rec.Before["Overview"] != "原有简介不能丢" {
		t.Errorf("快照的 Before.Overview = %v，期望「原有简介不能丢」", rec.Before["Overview"])
	}
	// 而且快照里必须已经写进了新值
	if formatSame(rec.After["Overview"], "原有简介不能丢") {
		t.Error("快照的 After.Overview 与旧值相同，说明 patch 没算对或记错了")
	}

	// 现在真的被改掉了
	m.mu.Lock()
	cur := m.items["m1"]
	m.mu.Unlock()
	if formatSame(cur["Overview"], "原有简介不能丢") {
		t.Fatal("刮削后 Overview 没变，测试前提不成立")
	}

	// 回滚
	out, err := app.rollbackItemSync(context.Background(), res.SnapshotID)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if out.ItemID != "m1" {
		t.Errorf("回滚结果里的条目 ID = %q，期望 m1", out.ItemID)
	}
	if !strings.Contains(out.Message, "图片") {
		t.Errorf("回滚提示必须说明图片不可还原，实际 %q", out.Message)
	}

	m.mu.Lock()
	back := m.items["m1"]
	m.mu.Unlock()
	for _, k := range rec.Changed {
		want := rec.Before[k]
		got := back[k]
		if want == nil || isNilVal(want) {
			if !isBlank(got) {
				t.Errorf("字段 %s 原来没有值，回滚后应是空，实际 %#v", k, got)
			}
			continue
		}
		if !formatSame(got, want) {
			t.Errorf("字段 %s 没还原：期望 %q，实际 %q", k, formatFieldValue(want), formatFieldValue(got))
		}
	}
	// 没进 patch 的字段不该被动
	if back["Type"] != "Movie" {
		t.Errorf("回滚动到了快照范围外的字段：Type = %v", back["Type"])
	}
}

// 「原来没有这个字段」也要能还原 —— 而清空列表字段必须发 `[]`：
// 发 null 会被 updateItem 的 nil 防护跳过，字段根本没被还原，界面却显示「已回滚」。
func TestItemRollbackClearsFieldsThatWereAbsent(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)

	m.items["m2"] = map[string]any{
		"Id": "m2", "Name": "SSIS-001", "Type": "Movie",
		// 没有 Overview、没有 Tags
		"ProviderIds": map[string]any{}, "ImageTags": map[string]any{},
	}
	res, err := app.ScrapeMovie(context.Background(), "m2", ScrapeOptions{})
	if err != nil {
		t.Fatalf("刮削失败: %v", err)
	}
	rec, _ := app.sync.Get(res.SnapshotID)
	// Name 原本就有值（"SSIS-001"），这里只关心**原本不存在**的那些字段。
	absent := []string{}
	for _, k := range rec.Changed {
		if rec.Before[k] == nil || isNilVal(rec.Before[k]) {
			absent = append(absent, k)
		}
	}
	if len(absent) == 0 {
		t.Fatal("测试前提不成立：这次刮削没有写到任何「原来不存在」的字段")
	}

	if _, err := app.rollbackItemSync(context.Background(), res.SnapshotID); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range absent {
		if !isBlank(m.items["m2"][k]) {
			t.Errorf("%s 原本为空，回滚后应清空，实际 %#v", k, m.items["m2"][k])
		}
	}
}

func TestItemRollbackTwiceRejected(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m3"] = map[string]any{
		"Id": "m3", "Name": "SSIS-001", "Type": "Movie",
		"ProviderIds": map[string]any{}, "ImageTags": map[string]any{},
	}
	res, err := app.ScrapeMovie(context.Background(), "m3", ScrapeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.rollbackItemSync(context.Background(), res.SnapshotID); err != nil {
		t.Fatalf("第一次回滚应成功: %v", err)
	}
	if _, err := app.rollbackItemSync(context.Background(), res.SnapshotID); err == nil {
		t.Error("同一条记录不该能回滚两次（否则第二次会用「写前值」再覆盖一遍，把后来手动改的东西抹掉）")
	}
}

// 人物资料的记录不能被条目回滚接口吃掉 —— 两边的字段语义完全不同
// （人物用 overview/premiere_date 这类 snake_case 键，条目用 Emby 的原始字段名）。
func TestItemRollbackRejectsPersonRecord(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	id := app.sync.Add(SyncRecord{
		PersonID: "p1", Name: "某演员",
		Before: map[string]any{"overview": "旧简介"},
		After:  map[string]any{"overview": "新简介"},
	})
	_, err := app.rollbackItemSync(context.Background(), id)
	if err == nil {
		t.Fatal("人物记录不该被条目回滚接口接受")
	}
	if !strings.Contains(err.Error(), "人物资料") {
		t.Errorf("错误信息应指出去演员资料里回滚，实际 %q", err.Error())
	}
}

// 升级前的历史记录没有 Kind 字段，必须按「人物资料」处理，不能丢。
func TestSyncRecordKindDefaultsToPerson(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	app.sync.Add(SyncRecord{PersonID: "p1", Name: "老记录"}) // 无 Kind
	app.sync.Add(SyncRecord{Kind: syncKindItem, ItemID: "i1", Name: "新条目"})

	if got := app.sync.ListKind(syncKindItem, 0); len(got) != 1 || got[0].ItemID != "i1" {
		t.Errorf("条目历史 = %+v，期望只有 i1", got)
	}
	if got := app.sync.ListKind(syncKindPerson, 0); len(got) != 1 || got[0].PersonID != "p1" {
		t.Errorf("人物历史 = %+v，期望只有 p1（没有 Kind 的老记录算人物）", got)
	}
	if got := app.sync.List(0); len(got) != 2 {
		t.Errorf("不过滤时应返回全部 2 条，实际 %d", len(got))
	}
	// 列表接口不该带快照本体（否则一次列表能回几百 KB）
	app.sync.Add(SyncRecord{Kind: syncKindItem, ItemID: "i2", Before: map[string]any{"Name": "x"}})
	for _, r := range app.sync.List(0) {
		if r.Before != nil || r.After != nil {
			t.Errorf("列表里带了快照本体：%+v", r)
		}
	}
}

// 手动编辑条目也要留快照，并且接口要把 snapshot_id 回给前端。
func TestHandleItemUpdateLeavesSnapshot(t *testing.T) {
	m := newMockEmby(t)
	app := testApp(t, m.srv.URL, "http://127.0.0.1:1")
	m.items["e1"] = map[string]any{
		"Id": "e1", "Name": "旧名字", "Type": "Movie",
		"Overview": "旧简介", "ProviderIds": map[string]any{}, "ImageTags": map[string]any{},
	}
	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/items/update", "application/json",
		strings.NewReader(`{"id":"e1","name":"新名字","overview":"新简介"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data struct {
			SnapshotID string `json:"snapshot_id"`
			Updated    []string
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Data.SnapshotID == "" {
		t.Fatal("手动编辑没有返回 snapshot_id")
	}

	rec, ok := app.sync.Get(out.Data.SnapshotID)
	if !ok || rec.ItemID != "e1" {
		t.Fatalf("快照记录不对：%+v ok=%v", rec, ok)
	}
	if rec.Before["Name"] != "旧名字" {
		t.Errorf("快照 Before.Name = %v，期望「旧名字」", rec.Before["Name"])
	}

	// 历史接口能列出来
	hresp, err := http.Get(srv.URL + "/api/items/history?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	var hist struct {
		Data struct {
			Records []SyncRecord `json:"records"`
			Note    string       `json:"note"`
		} `json:"data"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if len(hist.Data.Records) != 1 || hist.Data.Records[0].ID != out.Data.SnapshotID {
		t.Errorf("历史列表 = %+v", hist.Data.Records)
	}
	if !strings.Contains(hist.Data.Note, "图片") {
		t.Errorf("历史接口必须提示图片不可还原，实际 %q", hist.Data.Note)
	}

	// 回滚接口
	rresp, err := http.Post(srv.URL+"/api/items/rollback", "application/json",
		strings.NewReader(`{"id":"`+out.Data.SnapshotID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rresp.StatusCode != 200 {
		t.Fatalf("回滚 HTTP %d", rresp.StatusCode)
	}
	rresp.Body.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items["e1"]["Name"] != "旧名字" {
		t.Errorf("回滚后 Name = %v，期望「旧名字」", m.items["e1"]["Name"])
	}
	if m.items["e1"]["Overview"] != "旧简介" {
		t.Errorf("回滚后 Overview = %v，期望「旧简介」", m.items["e1"]["Overview"])
	}
}

// 国产传媒路径也要留快照，并且把 ID 透到结果里。
func TestCNScrapeLeavesRollbackSnapshot(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	m.mu.Lock()
	m.items["cn-snap"] = map[string]any{
		"Id": "cn-snap", "Name": "91CM-014", "Type": "Movie",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
		"Path": "/media/国产传媒/91CM-014/91CM-014.mp4",
	}
	m.mu.Unlock()

	opts := CNOptions{Fields: map[string]bool{"title": true, "tags": true}}
	res, err := app.ScrapeCN(context.Background(), "cn-snap", "", opts)
	if err != nil {
		t.Fatalf("刮削失败: %v", err)
	}
	if res.SnapshotID == "" {
		t.Fatal("国产传媒刮削没有留快照")
	}
	rec, ok := app.sync.Get(res.SnapshotID)
	if !ok || rec.kindOrDefault() != syncKindItem {
		t.Fatalf("快照记录不对：%+v", rec)
	}
	if len(rec.Changed) == 0 {
		t.Fatal("快照里没有记下改动的字段")
	}
	// 只勾了标题和标签，就不该出现日期字段
	for _, k := range rec.Changed {
		if k == "PremiereDate" || k == "ProductionYear" {
			t.Errorf("字段范围没收到勾选内：%v", rec.Changed)
		}
	}

	// 预演不该留快照
	pre, err := app.ScrapeCN(context.Background(), "cn-snap", "", CNOptions{
		Fields: map[string]bool{"title": true}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("预演失败: %v", err)
	}
	if pre.SnapshotID != "" {
		t.Errorf("预演没写入，不该有 snapshot_id，实际 %q", pre.SnapshotID)
	}
}

// 「原来没有值」时各类型的空值要对 —— 尤其列表必须给 `[]` 而不是 null。
func TestEmptyForItemField(t *testing.T) {
	cases := map[string]any{
		"Tags":           []any{},
		"Genres":         []any{},
		"ProviderIds":    map[string]any{},
		"ProductionYear": 0,
		"RunTimeTicks":   0,
		"Overview":       "",
		"咱们不认识的字段":       "",
	}
	for k, want := range cases {
		got := emptyForItemField(k)
		if !formatSame(got, want) || (want != "" && isNilVal(got)) {
			t.Errorf("emptyForItemField(%s) = %#v，期望 %#v", k, got, want)
		}
	}
	// 列表字段绝不能是 nil —— nil 会被 updateItem 跳过，字段就还原不了
	for k := range itemListFields {
		if isNilVal(emptyForItemField(k)) {
			t.Errorf("列表字段 %s 的空值不能是 nil（会被整对象替换的 nil 防护跳过）", k)
		}
	}
}
