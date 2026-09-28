package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 全空的、但每个密钥字段都填上独一无二的「哨兵值」的配置。
//
// 哨兵值刻意做得一眼能认出来（前缀 SECRET-）：任何一处漏脱敏，
// 断言就会把它指出来，而不是变成一句「包里好像有点不对」。
func diagSentinelConfig() Config {
	c := DefaultConfig()
	c.EmbyURL = "http://user:hunter2@emby.local:8096"
	c.Username = "admin"
	c.Password = "SECRET-EMBY-PASSWORD"
	c.APIKey = "SECRET-EMBY-APIKEY"
	c.Token = "SECRET-EMBY-TOKEN"
	c.MetaTubeURL = "http://mt.local:8080"
	c.MetaTubeToken = "SECRET-MT-TOKEN"
	c.JavBusURL = "https://www.javbus.com"
	c.JavBusCookie = "SECRET-JAVBUS-COOKIE"
	c.OpenAI.BaseURL = "https://api.example.com/v1"
	c.OpenAI.APIKey = "SECRET-OPENAI-KEY"
	c.Auth.Username = "admin"
	c.Auth.PasswordHash = "SECRET-PBKDF2-HASH"
	c.Proxy = "http://u:p@127.0.0.1:7890"
	return c
}

// 所有哨兵值（含被塞进 URL 里的 userinfo）。
var diagSentinels = []string{
	"SECRET-EMBY-PASSWORD", "SECRET-EMBY-APIKEY", "SECRET-EMBY-TOKEN",
	"SECRET-MT-TOKEN", "SECRET-JAVBUS-COOKIE", "SECRET-OPENAI-KEY",
	"SECRET-PBKDF2-HASH", "hunter2",
}

func diagTestInput() diagInput {
	return diagInput{
		Version: "v9.9.9",
		Config:  diagSentinelConfig(),
		History: []SyncRecord{
			{Kind: syncKindItem, ID: "r1", ItemID: "i1", Name: "某条目",
				CreatedAt: time.Now(), Sources: []string{"MetaTube 刮削"},
				Changed: []string{"Name"}, Before: map[string]any{"Name": "旧"},
				After: map[string]any{"Name": "新"}},
		},
		Jobs: []map[string]any{
			{"id": "j1", "kind": "scrape", "title": "批量刮削", "status": "done",
				"logs": []JobLog{{Time: time.Now(), Level: "info", Message: "HTTP 403"}}},
		},
		Cache: []diagCacheFile{{Name: "gfriends.json", Size: 6_500_000}},
	}
}

func diagZipText(t *testing.T, blob []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatalf("不是合法的 zip：%v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打不开 %s：%v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读 %s 失败：%v", f.Name, err)
		}
		out[f.Name] = string(b)
	}
	return out
}

// 这是这个文件里最重要的一条测试。
//
// 诊断包是**要被贴到公开 issue 里**的。漏一个密钥的后果是用户把
// Emby / MetaTube / OpenAI 的凭据公开了 —— 而且他不会知道。
func TestDiagBundleRedactsSecrets(t *testing.T) {
	blob, err := buildDiagZip(diagTestInput())
	if err != nil {
		t.Fatalf("打包失败：%v", err)
	}
	files := diagZipText(t, blob)
	all := ""
	for name, body := range files {
		if strings.Contains(body, "SECRET-") || strings.Contains(body, "hunter2") {
			for _, s := range diagSentinels {
				if strings.Contains(body, s) {
					t.Errorf("文件 %s 里出现了未脱敏的密钥 %q", name, s)
				}
			}
		}
		all += body
	}
	for _, s := range diagSentinels {
		if strings.Contains(all, s) {
			t.Errorf("诊断包里出现了未脱敏的密钥 %q", s)
		}
	}
	// 反过来，脱敏标记必须在 —— 否则「没有密钥」也可能是「整个配置都没打包」，
	// 测试会假绿。
	cfg := files["config.redacted.json"]
	if cfg == "" {
		t.Fatal("包里没有 config.redacted.json")
	}
	if !strings.Contains(cfg, "已脱敏") {
		t.Errorf("配置里没有任何脱敏标记，测试前提不成立：\n%s", cfg)
	}
	if !strings.Contains(cfg, `"password"`) {
		t.Error("配置里连 password 键都没有 —— 打包的可能不是真实配置")
	}
	// 非密钥字段要保留原值（否则这个包就没用了）
	if !strings.Contains(cfg, "mt.local:8080") {
		t.Error("MetaTube 地址这类非敏感信息应当保留，排障要看它")
	}
	if !strings.Contains(cfg, "admin") {
		t.Error("用户名应当保留（它本身不是凭据）")
	}
}

// 更严的一层：哨兵值不许出现在**任何**一个文件里，逐字节扫。
func TestDiagBundleNoSentinelAnywhere(t *testing.T) {
	blob, err := buildDiagZip(diagTestInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range diagSentinels {
		if bytes.Contains(blob, []byte(s)) {
			t.Errorf("整个 zip 字节流里仍能找到 %q", s)
		}
	}
}

// 键名模式匹配必须能自动覆盖「以后新加的密钥字段」——
// 这正是用模式而不是列举字段名的原因。
func TestDiagIsSecretKey(t *testing.T) {
	secret := []string{
		"password", "Password", "api_key", "apiKey", "APIKey",
		"metatube_token", "javbus_cookie", "client_secret",
		"password_hash", "refresh_credential", "authorization",
		"some_new_token_added_later", // 以后新加的字段
	}
	for _, k := range secret {
		if !diagIsSecretKey(k) {
			t.Errorf("%q 应被当成密钥", k)
		}
	}
	plain := []string{"username", "emby_url", "metatube_url", "concurrency",
		"javbus_interval_ms", "config_path", "data_dir", "cn_sites", "proxy"}
	for _, k := range plain {
		if diagIsSecretKey(k) {
			t.Errorf("%q 不该被当成密钥（脱敏过头会让包失去排障价值）", k)
		}
	}
}

// 嵌套结构也要递归脱敏 —— OpenAI 的 key 就在一层嵌套里。
func TestRedactConfigValueNested(t *testing.T) {
	in := map[string]any{
		"openai": map[string]any{
			"base_url": "https://api.example.com/v1",
			"api_key":  "SECRET-OPENAI-KEY",
		},
		"auth":     map[string]any{"password_hash": "SECRET-PBKDF2-HASH", "username": "admin"},
		"cn_sites": []any{map[string]any{"site": "xchina", "token": "SECRET-X"}},
	}
	out, ok := redactConfigValue(in).(map[string]any)
	if !ok {
		t.Fatal("返回值不是 map")
	}
	oai := out["openai"].(map[string]any)
	if oai["base_url"] != "https://api.example.com/v1" {
		t.Errorf("base_url 不该被改：%v", oai["base_url"])
	}
	if s, _ := oai["api_key"].(string); !strings.Contains(s, "已脱敏") {
		t.Errorf("嵌套的 api_key 没脱敏：%v", oai["api_key"])
	}
	auth := out["auth"].(map[string]any)
	if auth["username"] != "admin" {
		t.Errorf("用户名不该被改：%v", auth["username"])
	}
	if s, _ := auth["password_hash"].(string); !strings.Contains(s, "已脱敏") {
		t.Errorf("password_hash 没脱敏：%v", auth["password_hash"])
	}
	// 数组里的对象
	arr := out["cn_sites"].([]any)
	if s, _ := arr[0].(map[string]any)["token"].(string); !strings.Contains(s, "已脱敏") {
		t.Errorf("数组里对象的 token 没脱敏：%v", arr[0])
	}
}

// 「没设置」和「设置了」必须还能区分 —— 否则看不出「密钥压根没填」这种常见故障。
func TestDiagRedactMarkKeepsEmpty(t *testing.T) {
	if got := diagRedactMark(""); got != "" {
		t.Errorf("空值应保持空，实际 %q", got)
	}
	if got := diagRedactMark("abc"); !strings.Contains(got, "3") {
		t.Errorf("应带上长度（能看出「只填了 3 个字符」这种手抖），实际 %q", got)
	}
	if got := diagRedactMark(nil); got != "" {
		t.Errorf("nil 应返回空，实际 %q", got)
	}
}

// 键名带 password 但值是 **bool** 的开关不该被脱敏 —— 脱掉它反而看不出
// 「密码还是自动生成的那一个」这种最常见的排障信息。
// `password_generated` 就是真实存在的这种字段（实测过）。
func TestRedactKeepsBooleansUnderSecretNames(t *testing.T) {
	in := map[string]any{
		"auth": map[string]any{
			"password_generated": true,
			"password_hash":      "SECRET-PBKDF2-HASH",
		},
		"secrets":     map[string]any{"emby_api_key": true, "javbus_cookie": false},
		"concurrency": 8,
	}
	out := redactConfigValue(in).(map[string]any)
	auth := out["auth"].(map[string]any)
	if auth["password_generated"] != true {
		t.Errorf("bool 开关不该被脱敏：%v", auth["password_generated"])
	}
	if s, ok := auth["password_hash"].(string); !ok || !strings.Contains(s, "已脱敏") {
		t.Errorf("字符串密钥必须脱敏：%v", auth["password_hash"])
	}
	sec := out["secrets"].(map[string]any)
	if sec["emby_api_key"] != true || sec["javbus_cookie"] != false {
		t.Errorf("「已保存」布尔标记必须保留：%v", sec)
	}
	if out["concurrency"] != 8 {
		t.Errorf("普通数值不该被动：%v", out["concurrency"])
	}
}

func TestScrubURLCredentials(t *testing.T) {
	cases := map[string]string{
		"http://user:pass@127.0.0.1:7890": "127.0.0.1:7890",
		"http://127.0.0.1:7890":           "127.0.0.1:7890",
		"https://emby.local:8096":         "emby.local:8096",
		"":                                "",
	}
	for in, want := range cases {
		got := scrubURLCredentials(in)
		if want != "" && !strings.Contains(got, want) {
			t.Errorf("scrubURLCredentials(%q) = %q，应包含 %q", in, got, want)
		}
		if strings.Contains(got, "pass@") || strings.Contains(got, "user:") {
			t.Errorf("scrubURLCredentials(%q) = %q，userinfo 没被抹掉", in, got)
		}
	}
}

// 包的结构要对：README 说清有哪些文件、历史里不带快照本体、
// 任务日志带上了（排障最常看的就是它）。
func TestDiagBundleStructure(t *testing.T) {
	blob, err := buildDiagZip(diagTestInput())
	if err != nil {
		t.Fatal(err)
	}
	files := diagZipText(t, blob)
	for _, want := range []string{
		"README.txt", "info.txt", "config.redacted.json",
		"sync_history.json", "jobs.json", "cache.json",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("包里缺少 %s（有：%v）", want, diagKeysOf(files))
		}
	}
	if !strings.Contains(files["README.txt"], "脱敏") {
		t.Error("README 必须说明脱敏规则")
	}
	if !strings.Contains(files["info.txt"], "v9.9.9") {
		t.Error("info.txt 里应带上版本号")
	}
	if !strings.Contains(files["info.txt"], "运行时长") {
		t.Error("info.txt 里应带上运行时长")
	}
	if !strings.Contains(files["jobs.json"], "HTTP 403") {
		t.Error("任务日志必须带进包里 —— 排障最常看的就是它")
	}
	// 写入历史里不该有快照本体（Before/After 是条目元数据，属于「内容」）
	if strings.Contains(files["sync_history.json"], `"Before"`) ||
		strings.Contains(files["sync_history.json"], `"before"`) ||
		strings.Contains(files["sync_history.json"], `"After"`) ||
		strings.Contains(files["sync_history.json"], `"after"`) {
		t.Errorf("写入历史不该带快照本体：%s", files["sync_history.json"])
	}
	if !strings.Contains(files["sync_history.json"], "r1") {
		t.Error("写入历史里应有记录的元信息")
	}
	// cache.json 是可解析的清单
	var cache []diagCacheFile
	if err := json.Unmarshal([]byte(files["cache.json"]), &cache); err != nil {
		t.Errorf("cache.json 不是合法 JSON：%v", err)
	}
}

// 接口层：Content-Type 必须是 zip，且响应体里同样不能有密钥。
func TestHandleDiagBundle(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := app.store.Update(func(c *Config) {
		c.Password = "SECRET-EMBY-PASSWORD"
		c.APIKey = "SECRET-EMBY-APIKEY"
		c.JavBusCookie = "SECRET-JAVBUS-COOKIE"
		c.OpenAI.APIKey = "SECRET-OPENAI-KEY"
		c.Auth.PasswordHash = "SECRET-PBKDF2-HASH"
	}); err != nil {
		t.Fatal(err)
	}
	app.sync.Add(SyncRecord{Kind: syncKindItem, ItemID: "i1", Name: "某条目"})

	srv := httptest.NewServer(app.route())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/diag/bundle")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q，期望 application/zip", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Errorf("Content-Disposition = %q，应带上 zip 文件名", cd)
	}
	for _, s := range []string{"SECRET-EMBY-PASSWORD", "SECRET-EMBY-APIKEY",
		"SECRET-JAVBUS-COOKIE", "SECRET-OPENAI-KEY", "SECRET-PBKDF2-HASH"} {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("接口响应里出现了未脱敏的密钥 %q", s)
		}
	}
	// 并且是个真能打开的 zip
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
		t.Errorf("响应不是合法的 zip：%v", err)
	}
}

func diagKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
