package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 用真实服务端的返回形态起一个 mock MetaTube。
func newMockMT(t *testing.T, providersBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(providersBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// v1 实际返回的是 {"data":{"movie_providers":{名称:主页},...}}，不是扁平数组。
func TestMTProvidersObjectShape(t *testing.T) {
	body := `{"data":{"actor_providers":{"Gfriends":"https://github.com/gfriends/gfriends"},
	"movie_providers":{"FANZA":"https://www.dmm.co.jp/","JavBus":"https://www.javbus.com/","MGStage":"https://www.mgstage.com/"}}}`
	srv := newMockMT(t, body)
	list, err := NewMetaTube(Config{MetaTubeURL: srv.URL}).Providers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"FANZA", "JavBus", "MGStage"}
	if len(list) != len(want) {
		t.Fatalf("期望 %d 个 provider，实际 %d：%v", len(want), len(list), list)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Errorf("第 %d 个应为 %s，实际 %s（应已排序）", i, want[i], list[i])
		}
	}
}

func TestMTProvidersArrayShape(t *testing.T) {
	srv := newMockMT(t, `{"data":["FANZA","MGStage"]}`)
	list, err := NewMetaTube(Config{MetaTubeURL: srv.URL}).Providers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != "FANZA" {
		t.Errorf("扁平数组形式解析错误：%v", list)
	}
}

func TestMTProvidersWrappedShape(t *testing.T) {
	srv := newMockMT(t, `{"providers":["A","B"]}`)
	list, err := NewMetaTube(Config{MetaTubeURL: srv.URL}).Providers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("providers 包裹形式解析错误：%v", list)
	}
}

func TestMTProvidersUnrecognized(t *testing.T) {
	srv := newMockMT(t, `{"data":{"something_else":1}}`)
	_, err := NewMetaTube(Config{MetaTubeURL: srv.URL}).Providers(context.Background())
	if err == nil {
		t.Fatal("无法识别的结构应报错")
	}
	if !strings.Contains(err.Error(), "无法识别") {
		t.Errorf("错误信息应说明结构无法识别：%v", err)
	}
}

// plot / studio 要能兼容 plot|summary、studio|maker 两套字段名。
func TestMTMovieFieldAliases(t *testing.T) {
	var real MTMovie
	if err := json.Unmarshal([]byte(`{"maker":"エスワン","summary":"简介文本"}`), &real); err != nil {
		t.Fatal(err)
	}
	if real.plot() != "简介文本" {
		t.Errorf("summary 应映射到 plot()，实际 %q", real.plot())
	}
	if real.studio() != "エスワン" {
		t.Errorf("maker 应映射到 studio()，实际 %q", real.studio())
	}

	var legacy MTMovie
	if err := json.Unmarshal([]byte(`{"studio":"旧制作商","plot":"旧简介"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.plot() != "旧简介" || legacy.studio() != "旧制作商" {
		t.Errorf("旧字段名兼容失败：plot=%q studio=%q", legacy.plot(), legacy.studio())
	}

	// 两套都有时优先 plot / studio
	var both MTMovie
	_ = json.Unmarshal([]byte(`{"plot":"P","summary":"S","studio":"ST","maker":"MK"}`), &both)
	if both.plot() != "P" || both.studio() != "ST" {
		t.Errorf("同名冲突时应优先 plot/studio：%q %q", both.plot(), both.studio())
	}
}

// 端到端：真实形态的详情 JSON 应该能把 Overview / Studios 填进 Emby 的 patch。
func TestBuildItemPatchUsesRealMetaTubeFields(t *testing.T) {
	// 这段 JSON 是从真实 MetaTube 实例上抄下来的字段名
	raw := `{
		"id":"SSNI-989","number":"SSNI-989","provider":"JavBus",
		"title":"出張先の旅館で…","actors":["三上悠亜"],"genres":["巨乳","単体作品"],
		"cover_url":"https://www.javbus.com/pics/cover/83hf_b.jpg",
		"preview_images":["https://pics.dmm.co.jp/a.jpg","https://pics.dmm.co.jp/b.jpg"],
		"release_date":"2021-02-18T00:00:00Z","runtime":170,
		"director":"肉尊","maker":"エスワン ナンバーワンスタイル","label":"S1 NO.1 STYLE",
		"series":"","summary":"","big_cover_url":"","score":0
	}`
	var mv MTMovie
	if err := json.Unmarshal([]byte(raw), &mv); err != nil {
		t.Fatal(err)
	}
	patch := buildItemPatch(Item{"Id": "1", "Name": "SSNI-989"}, &mv, "SSNI-989")

	if patch["Name"] != "出張先の旅館で…" {
		t.Errorf("Name 错误：%v", patch["Name"])
	}
	if patch["SortName"] != "SSNI-989" {
		t.Errorf("SortName 错误：%v", patch["SortName"])
	}
	// Emby 用的是 .NET 往返格式（2021-02-18T00:00:00.0000000Z），只校验日期部分
	if d, _ := patch["PremiereDate"].(string); !strings.HasPrefix(d, "2021-02-18") {
		t.Errorf("PremiereDate 错误：%v", patch["PremiereDate"])
	}
	if patch["ProductionYear"] != 2021 {
		t.Errorf("ProductionYear 错误：%v", patch["ProductionYear"])
	}
	// 170 分钟 -> ticks
	if patch["RunTimeTicks"] != int64(170)*60*10_000_000 {
		t.Errorf("RunTimeTicks 错误：%v", patch["RunTimeTicks"])
	}

	// Studios 必须来自 maker（而不是空的 studio）
	studios, _ := patch["Studios"].([]map[string]any)
	if len(studios) == 0 {
		t.Fatalf("Studios 不应为空 —— maker 字段没映射上")
	}
	if studios[0]["Name"] != "エスワン ナンバーワンスタイル" {
		t.Errorf("Studios[0] 应为 maker 的值，实际 %v", studios[0]["Name"])
	}

	// People 要含演员和导演
	people, _ := patch["People"].([]map[string]any)
	types := map[string]string{}
	for _, p := range people {
		types[p["Name"].(string)] = p["Type"].(string)
	}
	if types["三上悠亜"] != "Actor" {
		t.Errorf("演员未写入：%v", people)
	}
	if types["肉尊"] != "Director" {
		t.Errorf("导演未写入：%v", people)
	}

	// ProviderIds 带上 MetaTube 标识
	ids, _ := patch["ProviderIds"].(map[string]any)
	if ids["MetaTube"] != "JavBus:SSNI-989" {
		t.Errorf("ProviderIds 错误：%v", patch["ProviderIds"])
	}
}
