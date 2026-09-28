package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- 名字相似度 ----------

func TestNameSimilarity(t *testing.T) {
	cases := []struct {
		a, b string
		want int // 精确值（0 表示只检查区间）
		min  int // want==0 时的下界
		max  int // want==0 时的上界
		note string
	}{
		{a: "三上悠亜", b: "三上悠亜", want: 100, note: "完全相同"},
		{a: "三上悠亜", b: " 三上悠亜 ", want: 100, note: "首尾空格"},
		{a: "三上悠亜", b: "三上　悠亜", want: 100, note: "全角空格"},
		{a: "Mikami Yua", b: "mikami yua", want: 100, note: "大小写"},
		{a: "三上悠亜", b: "三上悠亜(中文)", want: 90, note: "子串"},
		{a: "三上悠亜", b: "三上悠亞", min: 60, max: 85, note: "简繁异体：刻意不算重复"},
		{a: "三上悠亜", b: "深田えいみ", min: 0, max: 30, note: "完全不同的两个人"},
		{a: "", b: "三上悠亜", want: 0, note: "空名字"},
		{a: "AB", b: "ABCDEFG", min: 0, max: 95, note: "短前缀占比太低不算子串命中"},
	}
	for _, c := range cases {
		got := nameSimilarity(c.a, c.b)
		if c.want != 0 {
			if got != c.want {
				t.Errorf("nameSimilarity(%q, %q) = %d，期望 %d（%s）", c.a, c.b, got, c.want, c.note)
			}
			continue
		}
		if got < c.min || got > c.max {
			t.Errorf("nameSimilarity(%q, %q) = %d，期望在 [%d, %d]（%s）", c.a, c.b, got, c.min, c.max, c.note)
		}
	}
	// 必须对称 —— 否则同一批数据换个顺序分组结果就不一样了。
	if nameSimilarity("三上悠亜(中文)", "三上悠亜") != nameSimilarity("三上悠亜", "三上悠亜(中文)") {
		t.Error("相似度不对称")
	}
}

// ---------- 分簇 ----------

func noAlias(string) string { return "" }

func dupFixture() []dupPersonView {
	return []dupPersonView{
		{ID: "p1", Name: "三上悠亜"},
		{ID: "p2", Name: " 三上悠亜"},      // 只是多了个空格
		{ID: "p3", Name: "深田えいみ"},      // 无关的人
		{ID: "p4", Name: "深田えいみ "},     // 又是空格
		{ID: "p5", Name: "Mikami Yua"}, // 罗马字，与中文名无关（除非别名记着）
	}
}

func TestClusterDuplicatesByNormalizedName(t *testing.T) {
	groups := clusterDuplicates(dupFixture(), 86, noAlias)
	if len(groups) != 2 {
		t.Fatalf("应分成 2 组，实际 %d：%+v", len(groups), groups)
	}
	for _, g := range groups {
		if len(g.Persons) != 2 {
			t.Fatalf("每组应 2 人，实际 %d（%s）", len(g.Persons), g.Key)
		}
		if !strings.Contains(g.Reason, "完全相同") {
			t.Errorf("理由应说明是归一化后完全相同，实际 %q", g.Reason)
		}
	}
	// 分不清同名同姓的两个不同人：这是字符串层面能做到的极限，
	// 界面必须让用户自己确认（这条注释是为了防止以后有人想「自动全并」）。
}

func TestClusterDuplicatesDoesNotMergeSimilarButDistinct(t *testing.T) {
	items := []dupPersonView{
		{ID: "a", Name: "三上悠亜"},
		{ID: "b", Name: "三上亜衣"}, // 同姓但不同人
	}
	if groups := clusterDuplicates(items, 86, noAlias); len(groups) != 0 {
		t.Errorf("默认阈值下不该把「三上悠亜」和「三上亜衣」并成一组：%+v", groups)
	}
}

func TestClusterDuplicatesUsesAliasMemory(t *testing.T) {
	items := []dupPersonView{
		{ID: "a", Name: "三上悠亜"},
		{ID: "b", Name: "Mikami Yua"}, // 罗马字，字符串上毫不相干
	}
	canon := func(name string) string {
		if normName(name) == "三上悠亜" || normName(name) == "mikamiyua" {
			return "三上悠亜"
		}
		return ""
	}
	groups := clusterDuplicates(items, 86, canon)
	if len(groups) != 1 || len(groups[0].Persons) != 2 {
		t.Fatalf("别名记忆应把这两个名字并起来，实际 %+v", groups)
	}
	if !strings.Contains(groups[0].Reason, "别名记忆") {
		t.Errorf("理由应提到别名记忆，实际 %q", groups[0].Reason)
	}
	if groups[0].Key != "三上悠亜" {
		t.Errorf("组名应取别名组里的规范名，实际 %q", groups[0].Key)
	}
}

func TestClusterDuplicatesSkipsHugeBuckets(t *testing.T) {
	// 80 个「同一个首字符 + 同长度」的不同名字：桶太大就不做两两比较，
	// 否则万级人物库上这一步会变成 O(n²)。
	items := make([]dupPersonView, 0, 200)
	for i := 0; i < 120; i++ {
		// 用汉字造一批 3 字名字，首字都是「三」
		name := "三" + string(rune('一'+i%20)) + string(rune('亜'+i/20))
		items = append(items, dupPersonView{ID: "p" + itoa(i), Name: name})
	}
	groups := clusterDuplicates(items, 86, noAlias)
	for _, g := range groups {
		if len(g.Persons) > 2 {
			t.Fatalf("桶超限时不该产生大组：%d 人（%s）", len(g.Persons), g.Key)
		}
	}
	// 关键断言：**不能挂**。真实库里这个桶可能有上千个名字。
}

func TestClusterDuplicatesSortsByWorks(t *testing.T) {
	items := []dupPersonView{
		{ID: "small", Name: "深田えいみ", Works: 3},
		{ID: "big", Name: "深田えいみ ", Works: 80},
	}
	groups := clusterDuplicates(items, 86, noAlias)
	if len(groups) != 1 {
		t.Fatalf("应合成一组，实际 %d", len(groups))
	}
	if groups[0].Persons[0].ID != "big" {
		t.Errorf("作品多的应排在前面（更可能是正主），实际 %q 在前", groups[0].Persons[0].ID)
	}
}

// ---------- People 改写 ----------

func TestRepointPeople(t *testing.T) {
	raw := []any{
		map[string]any{"Id": "drop", "Name": "三上悠亜 ", "Type": "Actor", "Role": ""},
		map[string]any{"Id": "other", "Name": "别人", "Type": "Actor"},
	}
	out, changed := repointPeople(raw, "drop", "keep", "三上悠亜")
	if !changed {
		t.Fatal("应该有改动")
	}
	if len(out) != 2 {
		t.Fatalf("条数不该变，实际 %d", len(out))
	}
	first := out[0].(map[string]any)
	if itemStr(first, "Id") != "keep" || itemStr(first, "Name") != "三上悠亜" {
		t.Errorf("Id/Name 没换成 keep：%+v", first)
	}
	// Type / Role 必须保留 —— 整条替换会抹掉「这个人是导演」或角色名。
	if itemStr(first, "Type") != "Actor" {
		t.Errorf("Type 丢了：%+v", first)
	}
	if _, ok := first["Role"]; !ok {
		t.Errorf("Role 键丢了：%+v", first)
	}
	// 原来的入参不该被就地改动
	if itemStr(raw[0].(map[string]any), "Id") != "drop" {
		t.Error("不该就地改动入参")
	}
}

func TestRepointPeopleNoChange(t *testing.T) {
	raw := []any{map[string]any{"Id": "other", "Name": "别人"}}
	if _, changed := repointPeople(raw, "drop", "keep", "三上悠亜"); changed {
		t.Error("没有指向 drop 的引用时不该报告有改动")
	}
	if _, changed := repointPeople(nil, "drop", "keep", "x"); changed {
		t.Error("nil 输入不该报告有改动")
	}
	if _, changed := repointPeople([]any{"字符串不是对象"}, "drop", "keep", "x"); changed {
		t.Error("非对象元素不该报告有改动")
	}
}

func TestRepointPeopleDedupsIdenticalEntries(t *testing.T) {
	raw := []any{
		map[string]any{"Id": "drop", "Name": "A", "Type": "Actor", "Role": ""},
		map[string]any{"Id": "keep", "Name": "A", "Type": "Actor", "Role": ""},
	}
	out, _ := repointPeople(raw, "drop", "keep", "A")
	if len(out) != 1 {
		t.Errorf("换完之后两条完全一样，应去重成 1 条，实际 %d", len(out))
	}
}

// ---------- 端到端（mock Emby） ----------

func mergeFixture(t *testing.T) *mockEmby {
	t.Helper()
	m := newMockEmby(t)
	m.persons = []Person{
		{Id: "keep", Name: "三上悠亜"},
		{Id: "drop", Name: "三上悠亜 ", ImageTags: map[string]string{"Primary": "tag-drop"}},
	}
	m.personParent["keep"] = ""
	m.personParent["drop"] = ""
	// 人物条目本身也放在 items 里 —— ItemDetail（读 /Items/{id}）就是从这读的，
	// 而归并要用它拿「保留那个人的名字」和「谁有头像」。
	m.items["keep"] = map[string]any{"Id": "keep", "Name": "三上悠亜", "Type": "Person"}
	m.items["drop"] = map[string]any{
		"Id": "drop", "Name": "三上悠亜 ", "Type": "Person",
		"ImageTags": map[string]any{"Primary": "tag-drop"},
	}
	// 两部片子挂在 drop 名下（其中一部同时有别人，验证只换该换的那条）
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001", "Type": "Movie",
		"People": []any{map[string]any{"Id": "drop", "Name": "三上悠亜 ", "Type": "Actor"}},
	}
	m.items["m2"] = map[string]any{
		"Id": "m2", "Name": "SSIS-002", "Type": "Movie",
		"People": []any{
			map[string]any{"Id": "drop", "Name": "三上悠亜 ", "Type": "Actor"},
			map[string]any{"Id": "other", "Name": "别人", "Type": "Actor"},
		},
	}
	return m
}

func TestPersonMergeDryRunWritesNothing(t *testing.T) {
	m := mergeFixture(t)
	app := testApp(t, m.srv.URL, "")
	e := NewEmby(app.store.Get())

	res, err := app.runPersonMerge(context.Background(), e, "keep", []string{"drop"}, true)
	if err != nil {
		t.Fatalf("预演失败：%v", err)
	}
	if res.TotalItems != 2 {
		t.Errorf("预演应算出涉及 2 部作品，实际 %d", res.TotalItems)
	}
	if len(res.Plan) != 1 || res.Plan[0].Moved != 2 {
		t.Errorf("预演计划不对：%+v", res.Plan)
	}
	if !res.Plan[0].Image {
		t.Error("keep 没头像、drop 有头像，预演应指出会转移头像")
	}
	// 一个字节都没写
	if len(m.patched) != 0 {
		t.Errorf("预演不该写 Emby，实际改了 %d 个条目", len(m.patched))
	}
	if len(m.deletedItems) != 0 {
		t.Errorf("预演不该删条目，实际删了 %v", m.deletedItems)
	}
	if len(m.uploaded) != 0 {
		t.Errorf("预演不该传图，实际传了 %v", m.uploaded)
	}
	if len(app.sync.ListKind(syncKindItem, 10)) != 0 {
		t.Error("预演不该留写入快照")
	}
}

func TestPersonMergeMovesWorksTransfersAvatarAndDeletes(t *testing.T) {
	m := mergeFixture(t)
	app := testApp(t, m.srv.URL, "")
	e := NewEmby(app.store.Get())

	res, err := app.runPersonMerge(context.Background(), e, "keep", []string{"drop"}, false)
	if err != nil {
		t.Fatalf("归并失败：%v", err)
	}
	if res.TotalItems != 2 || res.Deleted != 1 {
		t.Fatalf("结果不对：%+v", res)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("不该有错误：%v", res.Errors)
	}

	// 1) 作品改挂到 keep，Name 也跟着换
	for _, id := range []string{"m1", "m2"} {
		it := m.items[id]
		if !itemHasPerson(it, map[string]bool{"keep": true}) {
			t.Errorf("%s 没有改挂到 keep：%+v", id, it["People"])
		}
		if itemHasPerson(it, map[string]bool{"drop": true}) {
			t.Errorf("%s 还留着 drop 的引用：%+v", id, it["People"])
		}
	}
	// 别人不该被顺手改掉
	if !itemHasPerson(m.items["m2"], map[string]bool{"other": true}) {
		t.Error("m2 里另一位演员被误改了")
	}
	// 名字要换成 keep 的（Emby 详情页显示的是 People 里的 Name）
	people := m.items["m1"]["People"].([]any)
	if itemStr(people[0].(map[string]any), "Name") != "三上悠亜" {
		t.Errorf("People.Name 没换成 keep 的名字：%+v", people[0])
	}

	// 2) 头像转移
	if len(m.uploaded) == 0 || !strings.HasPrefix(m.uploaded[0], "keep/Primary/") {
		t.Errorf("头像没转到 keep：%v", m.uploaded)
	}
	if len(m.imageGets) == 0 || !strings.HasPrefix(m.imageGets[0], "drop/Primary") {
		t.Errorf("头像应从 drop 取：%v", m.imageGets)
	}

	// 3) drop 条目被删
	if len(m.deletedItems) != 1 || m.deletedItems[0] != "drop" {
		t.Errorf("drop 条目没删掉：%v", m.deletedItems)
	}

	// 4) 每部作品写入前都留了快照 —— 事后还能把 People 回滚回去
	recs := app.sync.ListKind(syncKindItem, 10)
	if len(recs) != 2 {
		t.Fatalf("应为 2 部作品各留一条快照，实际 %d", len(recs))
	}
	for _, rec := range recs {
		if rec.ItemID != "m1" && rec.ItemID != "m2" {
			t.Errorf("快照的 ItemID 不对：%+v", rec)
		}
	}
}

// 回滚能把 People 还原成「原来挂在谁名下」—— 这是这套东西唯一的后悔药。
// （被删掉的人物条目不会自动重建，界面上写明了。）
func TestPersonMergeSnapshotCanRollBackPeople(t *testing.T) {
	m := mergeFixture(t)
	app := testApp(t, m.srv.URL, "")
	e := NewEmby(app.store.Get())

	res, err := app.runPersonMerge(context.Background(), e, "keep", []string{"drop"}, false)
	if err != nil {
		t.Fatalf("归并失败：%v", err)
	}
	var snapID string
	for _, r := range app.sync.ListKind(syncKindItem, 10) {
		if r.ItemID == "m1" {
			snapID = r.ID
		}
	}
	if snapID == "" {
		t.Fatal("找不到 m1 的快照")
	}
	if _, err := app.rollbackItemSync(context.Background(), snapID); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	if !itemHasPerson(m.items["m1"], map[string]bool{"drop": true}) {
		t.Errorf("回滚后 m1 应挂回 drop，实际 %+v", m.items["m1"]["People"])
	}
	_ = res
}

// 查重接口：只读，且要把候选按组返回。
func TestHandlePersonDuplicates(t *testing.T) {
	m := mergeFixture(t)
	m.persons = append(m.persons, Person{Id: "other", Name: "深田えいみ"})
	app := testApp(t, m.srv.URL, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/persons/duplicates?min_score=86", nil)
	app.handlePersonDuplicates(rec, req)
	if rec.Code != 200 {
		t.Fatalf("HTTP %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data dupReportView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if len(env.Data.Groups) != 1 {
		t.Fatalf("应只找出「三上悠亜」这一组，实际 %d：%+v", len(env.Data.Groups), env.Data.Groups)
	}
	g := env.Data.Groups[0]
	if len(g.Persons) != 2 {
		t.Fatalf("组里应有 2 个候选，实际 %d", len(g.Persons))
	}
	// 作品数要算出来（排序和判断「哪个是正主」都靠它）
	if g.Persons[0].ID != "drop" || g.Persons[0].Works != 2 {
		t.Errorf("作品多的那个应排在最前且作品数=2，实际 %+v", g.Persons[0])
	}
	// 只读：一个字节都没写
	if len(m.patched) != 0 || len(m.deletedItems) != 0 {
		t.Error("查重不该写任何东西")
	}
}
