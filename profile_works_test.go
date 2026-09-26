package main

// 「这个演员在媒体库里有哪些作品」（PersonWorks）的回归测试。
//
// 这段逻辑真正容易出错的地方不是查询本身，而是**归类与过滤**：
//   - 条目里根本没有库名，只能靠磁盘路径前缀去对；对错前缀就会把作品
//     归到别的库，甚至把 `/data/movies-archive` 当成 `/data/movies`。
//   - 库查不到时必须降级（少个标签），而不是让整个列表失败。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// worksFixture 造一个演员 + 若干部作品，分布在两个库的磁盘路径下。
func worksFixture(t *testing.T, m *mockEmby) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.libFolders = []LibraryFolder{
		{Name: "骑兵有码", ItemID: "502849", Locations: []string{"/data/strm/女优库", "/data/strm/有码更新"}},
		{Name: "步兵无码", ItemID: "502852", Locations: []string{"/data/strm/无码更新"}},
	}
	people := []any{map[string]any{"Id": "p-1", "Name": "三島奈津子", "Type": "Actor"}}
	m.items["508921"] = map[string]any{
		"Id": "508921", "Name": "DOCP-095 巨乳继姊泡泡浴体验", "Type": "Movie",
		"ProductionYear": float64(2018), "PremiereDate": "2018-10-04T16:00:00.0000000Z",
		"Path":      "/data/strm/女优库/三島奈津子/DOCP-095.strm",
		"ImageTags": map[string]any{"Primary": "tag-abc"},
		"People":    people,
	}
	m.items["512550"] = map[string]any{
		"Id": "512550", "Name": "GETS-113 用利尿剂让美脚女教师失禁", "Type": "Movie",
		"ProductionYear": float64(2019), "PremiereDate": "2019-02-01T16:00:00.0000000Z",
		"Path":   "/data/strm/无码更新/GETS-113.strm",
		"People": people,
	}
	// 同一个演员的另一个人条目 —— 不该出现在「作品」里
	m.items["p-1"] = map[string]any{"Id": "p-1", "Name": "三島奈津子", "Type": "Person"}
	// 别的演员的作品 —— 不该出现
	m.items["599999"] = map[string]any{
		"Id": "599999", "Name": "ABCD-001 别人的片", "Type": "Movie",
		"Path":   "/data/strm/女优库/别的演员/ABCD-001.strm",
		"People": []any{map[string]any{"Id": "p-2", "Name": "别人", "Type": "Actor"}},
	}
}

func findWork(items []PersonWork, id string) *PersonWork {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func TestPersonWorksMapsFieldsAndLibrary(t *testing.T) {
	m := newMockEmby(t)
	worksFixture(t, m)
	a := newProfileTestApp(t, m)

	res, err := a.PersonWorks(context.Background(), "p-1", 0, 60)
	if err != nil {
		t.Fatalf("PersonWorks: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("该演员应有 2 部作品，实际 %d：%+v", len(res.Items), res.Items)
	}

	w := findWork(res.Items, "508921")
	if w == nil {
		t.Fatal("没找到 DOCP-095")
	}
	if w.Number != "DOCP-095" {
		t.Errorf("番号应从条目名里算出来，实际 %q", w.Number)
	}
	if w.Year != 2018 {
		t.Errorf("年份错误：%d", w.Year)
	}
	if w.Library != "骑兵有码" {
		t.Errorf("库名应由磁盘路径前缀推出，实际 %q", w.Library)
	}
	if w.ImageTag != "tag-abc" {
		t.Errorf("封面 tag 没带出来，实际 %q", w.ImageTag)
	}

	w2 := findWork(res.Items, "512550")
	if w2 == nil {
		t.Fatal("没找到 GETS-113")
	}
	if w2.Library != "步兵无码" {
		t.Errorf("第二个库归类错误：%q", w2.Library)
	}
}

// 库查不到时要降级成「没有库名」，而不是整个请求失败 —— 少一个标签而已。
func TestPersonWorksSurvivesLibraryLookupFailure(t *testing.T) {
	m := newMockEmby(t)
	worksFixture(t, m)
	m.failLibFolders = true
	a := newProfileTestApp(t, m)

	res, err := a.PersonWorks(context.Background(), "p-1", 0, 60)
	if err != nil {
		t.Fatalf("库信息取不到不该让整个请求失败：%v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("作品列表仍应完整，实际 %d", len(res.Items))
	}
	for _, w := range res.Items {
		if w.Library != "" {
			t.Errorf("库信息取不到时不该编一个库名：%q", w.Library)
		}
	}
}

func TestPersonWorksRejectsEmptyID(t *testing.T) {
	m := newMockEmby(t)
	a := newProfileTestApp(t, m)
	if _, err := a.PersonWorks(context.Background(), "  ", 0, 60); err == nil {
		t.Fatal("空演员 ID 应当直接报错")
	}
}

// libraryOfPath 的前缀匹配必须按**路径段**对齐。
//
// 直接看字符串前缀的话，`/data/movies-archive/x.mp4` 会被当成 `/data/movies`
// 这个库里的东西 —— 库名显示错，用户按库筛选就全乱了。
func TestLibraryOfPathSegmentsAndLongestPrefix(t *testing.T) {
	roots := []libraryRoot{
		{prefix: "/data/movies", name: "电影"},
		{prefix: "/data/movies/4K", name: "4K 专区"},
	}
	cases := []struct {
		path string
		want string
	}{
		{"/data/movies/a.mp4", "电影"},
		{"/data/movies/4K/a.mp4", "4K 专区"}, // 长前缀优先（roots 没排序也要对）
		{"/data/movies-archive/a.mp4", ""}, // 不能按字符串前缀误判
		{"/data/moviesx/a.mp4", ""},        // 同上
		{"/data/Movies/a.mp4", "电影"},       // 大小写不敏感（Windows 路径）
		{"/data/movies", "电影"},             // 路径就是库根
		{"/other/a.mp4", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := libraryOfPath(roots, c.path); got != c.want {
			t.Errorf("libraryOfPath(%q) = %q，期望 %q", c.path, got, c.want)
		}
	}
}

// handler 层的入参校验：没有演员 ID 必须 400，而不是去查一个空 ID。
func TestHandleProfileWorksRequiresPersonID(t *testing.T) {
	m := newMockEmby(t)
	a := newProfileTestApp(t, m)

	rec := httptest.NewRecorder()
	a.handleProfileWorks(rec, httptest.NewRequest(http.MethodGet, "/api/profile/works?person_id=", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 person_id 应返回 400，实际 %d", rec.Code)
	}

	worksFixture(t, m)
	rec = httptest.NewRecorder()
	a.handleProfileWorks(rec, httptest.NewRequest(http.MethodGet, "/api/profile/works?person_id=p-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("正常查询应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Total int          `json:"total"`
			Items []PersonWork `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Total != 2 || len(out.Data.Items) != 2 {
		t.Errorf("应返回 2 部作品，实际 total=%d items=%d", out.Data.Total, len(out.Data.Items))
	}
}
