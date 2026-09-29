package main

// 「演员资料完整度」——头像页上那个百分比的来路。
//
// 分母固定 5，对应「演员资料」面板逐字段比对的五行（简介 / 出生日期 / 出生年份 /
// 出生地 / 外部 ID）。这里守三件事：
//   1. 判空口径：空白串、0、空数组、空 map 都不算「已填」
//   2. Person → Item 的转换不漏字段（漏了只会让百分比偏低，不报错）
//   3. 接口真的算了并下发，且**请求 Emby 时带齐了 Fields**
//
// 第 3 条的后半截最要紧：实测 Emby 不带 Fields 时一个资料字段都不返回
// （返回体只剩 BackdropImageTags / Id / ImageTags / Name / ServerId / Type），
// 所以「忘了把新字段加进 Fields」在线上表现为「所有人都是 0%」这种静默失效。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProfileCompleteness 逐项验证「一个字段填上 = 加一分」。
//
// 将来 embyProfileSnapshot 改了键名，这里会立刻红 —— 而不是变成「百分比
// 悄悄少一档」那种要靠肉眼发现的错。
func TestProfileCompleteness(t *testing.T) {
	cases := []struct {
		name   string
		item   Item
		filled int
	}{
		{"什么都没有", Item{}, 0},
		{"只有简介", Item{"Overview": "某人"}, 1},
		{"简介是空白字符不算", Item{"Overview": "  \n\t "}, 0},
		{"出生日期", Item{"PremiereDate": "1994-12-26T00:00:00"}, 1},
		{"出生年份", Item{"ProductionYear": 1994}, 1},
		{"出生年份为 0 不算", Item{"ProductionYear": 0}, 0},
		{"出生地", Item{"ProductionLocations": []string{"日本·東京都"}}, 1},
		{"出生地是空数组不算", Item{"ProductionLocations": []string{}}, 0},
		{"外部 ID", Item{"ProviderIds": map[string]string{"Tmdb": "2139451"}}, 1},
		{"外部 ID 是空 map 不算", Item{"ProviderIds": map[string]string{}}, 0},
		{"无关字段不参与", Item{"Name": "某人", "Id": "p1", "ImageTags": map[string]string{"Primary": "t"}}, 0},
		{
			"五项齐全",
			Item{
				"Overview":            "某人",
				"PremiereDate":        "1994-12-26T00:00:00",
				"ProductionYear":      1994,
				"ProductionLocations": []string{"日本·東京都"},
				"ProviderIds":         map[string]string{"Tmdb": "2139451"},
			},
			5,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filled, total := profileCompleteness(c.item)
			if total != 5 {
				t.Fatalf("分母应固定为 5，实际 %d", total)
			}
			if filled != c.filled {
				t.Errorf("已填应为 %d，实际 %d", c.filled, filled)
			}
		})
	}
}

// TestPersonProfileItemKeepsFields 守 Person.profileItem 不漏字段。
//
// 列表接口走的是 profileItem → embyProfileSnapshot 这条路，漏一个字段的表现是
// 「完整度永远偏低」而不是报错 —— 用真实返回值对不出来的那种错。
func TestPersonProfileItemKeepsFields(t *testing.T) {
	p := Person{
		Overview:            "简介",
		PremiereDate:        "1994-12-26T00:00:00",
		ProductionYear:      1994,
		ProductionLocations: []string{"日本·東京都"},
		ProviderIds:         map[string]string{"Tmdb": "2139451"},
	}
	filled, total := profileCompleteness(p.profileItem())
	if total != 5 || filled != 5 {
		t.Fatalf("profileItem 应保住全部 5 个字段，实际 %d/%d", filled, total)
	}
}

// TestHandlePersonsReturnsProfilePercent 断言接口把完整度算出来并下发。
func TestHandlePersonsReturnsProfilePercent(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)

	full := Person{
		Id: "p-full", Name: "资料齐全",
		Overview:            "简介",
		PremiereDate:        "1994-12-26T00:00:00",
		ProductionYear:      1994,
		ProductionLocations: []string{"日本·東京都"},
		ProviderIds:         map[string]string{"Tmdb": "2139451"},
	}
	// 有出生日期就会有出生年份（写入时年份是从生日派生的），所以 2/5 是线上
	// 最常见的那一档：只补过生日、没补简介和出生地。
	half := Person{Id: "p-half", Name: "只有简介和生日", Overview: "简介", PremiereDate: "1994-12-26T00:00:00"}
	none := Person{Id: "p-none", Name: "什么都没"}
	m.persons = []Person{full, half, none}

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/persons?limit=10")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Data struct {
			Items []struct {
				Name           string `json:"Name"`
				ProfileFilled  int    `json:"profile_filled"`
				ProfileTotal   int    `json:"profile_total"`
				ProfilePercent int    `json:"profile_percent"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Data.Items) != 3 {
		t.Fatalf("应有 3 条，实际 %d", len(out.Data.Items))
	}

	want := []struct {
		name    string
		filled  int
		percent int
	}{
		{"资料齐全", 5, 100},
		{"只有简介和生日", 2, 40},
		{"什么都没", 0, 0},
	}
	for i, w := range want {
		got := out.Data.Items[i]
		if got.Name != w.name {
			t.Fatalf("第 %d 条应是 %q，实际 %q", i, w.name, got.Name)
		}
		if got.ProfileTotal != 5 {
			t.Errorf("%s：分母应为 5，实际 %d", w.name, got.ProfileTotal)
		}
		if got.ProfileFilled != w.filled {
			t.Errorf("%s：已填应为 %d，实际 %d", w.name, w.filled, got.ProfileFilled)
		}
		if got.ProfilePercent != w.percent {
			t.Errorf("%s：百分比应为 %d%%，实际 %d%%", w.name, w.percent, got.ProfilePercent)
		}
	}
}

// TestHandlePersonsRequestsProfileFields 守「请求 Emby 时把资料字段列全了」。
//
// 单独一条，因为它守的失效形态和上面完全不同：百分比算错会显示一个**看得出**
// 的错数；而 **Fields 少列字段是「一致地错」** —— 界面与接口彼此对得上，只是
// 一致偏低。真实 Emby 只返回 Fields 里列出的字段（实测不带 Fields 时连
// ProviderIds 都没有），而 mock 不做这个裁剪，所以只能靠这条断言拦住。
func TestHandlePersonsRequestsProfileFields(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.persons = []Person{{Id: "p1", Name: "甲"}}

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/persons?limit=10")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	for _, want := range []string{
		"Overview", "PremiereDate", "ProductionYear", "ProductionLocations",
		"ProviderIds", "ImageTags",
	} {
		if !strings.Contains(m.lastPersonFields, want) {
			t.Errorf("/Persons 的 Fields 少了 %q（实际 %q）—— 少了它 Emby 就不会返回该字段，完整度会静默偏低",
				want, m.lastPersonFields)
		}
	}
}
