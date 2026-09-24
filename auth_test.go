package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------- 密码派生 ----------

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("哈希里不该出现明文密码")
	}
	if !strings.HasPrefix(hash, authAlgo+"$") {
		t.Errorf("哈希带算法前缀，换算法时老密码才不至于失效：%s", hash)
	}
	if !verifyPassword(hash, "correct horse battery staple") {
		t.Error("正确密码应当校验通过")
	}
	if verifyPassword(hash, "correct horse battery stapl") {
		t.Error("错一个字符就不该通过")
	}
	if verifyPassword(hash, "") {
		t.Error("空密码不该通过")
	}
}

func TestPasswordHashSaltIsRandom(t *testing.T) {
	a, _ := hashPassword("same-password")
	b, _ := hashPassword("same-password")
	if a == b {
		t.Error("同样的密码两次派生不该一样（盐是随机的）—— 否则彩虹表就能复用")
	}
	if !verifyPassword(a, "same-password") || !verifyPassword(b, "same-password") {
		t.Error("两个哈希都应当能校验通过")
	}
}

func TestVerifyPasswordRejectsBadHash(t *testing.T) {
	cases := map[string]string{
		"空串":         "",
		"字段不够":       "pbkdf2-sha256$abc",
		"算法不认识":      "bcrypt$1000$c2FsdA$aGFzaA",
		"轮数不是数字":     "pbkdf2-sha256$notanumber$c2FsdA$aGFzaA",
		"轮数小得离谱":     "pbkdf2-sha256$10$c2FsdA$aGFzaA",
		"盐不是 base64": "pbkdf2-sha256$1000$!!!!$aGFzaA",
		"派生值太短":      "pbkdf2-sha256$1000$c2FsdA$YQ",
	}
	for name, bad := range cases {
		if verifyPassword(bad, "whatever") {
			t.Errorf("%s：坏哈希必须一律返回 false，不能 panic 也不能放行", name)
		}
	}
}

// 轮数是写在哈希串里的，改了轮数就应当校验失败（防止把 200k 轮的哈希改成 1 轮再来爆破）。
func TestVerifyPasswordUsesStoredIterations(t *testing.T) {
	hash, err := hashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(hash, "$"+itoa(authIterations)+"$", "$2000$", 1)
	if tampered == hash {
		t.Fatal("测试没构造出被改过的哈希")
	}
	if verifyPassword(tampered, "pw") {
		t.Error("轮数被改过之后不该还能验证通过")
	}
}

// ---------- 随机密码 ----------

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw := randomPassword(authPwdLen)
		if len(pw) != authPwdLen {
			t.Fatalf("长度应为 %d，实际 %d：%q", authPwdLen, len(pw), pw)
		}
		for _, r := range pw {
			if !strings.ContainsRune(authPwdAlphabet, r) {
				t.Fatalf("出现了字母表外的字符 %q：%q", r, pw)
			}
		}
		// 去掉易混字符是为了让用户能照着控制台抄对
		if strings.ContainsAny(pw, "0O1lI") {
			t.Errorf("不该出现易混字符：%q", pw)
		}
		if seen[pw] {
			t.Fatalf("生成了重复的密码：%q", pw)
		}
		seen[pw] = true
	}
}

// ---------- 会话 ----------

func TestSessionsIssueAndValidate(t *testing.T) {
	s := newSessions()
	tok := s.issue("admin")
	if len(tok) != 64 {
		t.Errorf("会话令牌应当是 32 字节（64 个 hex 字符），实际 %d", len(tok))
	}
	sess, ok := s.valid(tok)
	if !ok || sess.user != "admin" {
		t.Fatalf("刚签发的会话应当有效：%v %v", sess, ok)
	}
	if _, ok := s.valid(""); ok {
		t.Error("空令牌不该有效")
	}
	if _, ok := s.valid("deadbeef"); ok {
		t.Error("不存在的令牌不该有效")
	}
	s.drop(tok)
	if _, ok := s.valid(tok); ok {
		t.Error("注销之后令牌应当立即失效")
	}
}

func TestSessionsExpire(t *testing.T) {
	s := newSessions()
	tok := s.issue("admin")
	// 直接把会话改成已过期（不 sleep 7 天）
	s.mu.Lock()
	s.m[tok].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if _, ok := s.valid(tok); ok {
		t.Error("过期会话不该有效")
	}
}

func TestSessionsDropOthers(t *testing.T) {
	s := newSessions()
	keep := s.issue("admin")
	a := s.issue("admin")
	b := s.issue("admin")
	s.dropOthers(keep)
	if _, ok := s.valid(keep); !ok {
		t.Error("当前会话应当被保留（否则改完密码自己也被踢下线）")
	}
	if _, ok := s.valid(a); ok {
		t.Error("其他会话应当被踢掉")
	}
	if _, ok := s.valid(b); ok {
		t.Error("其他会话应当被踢掉")
	}
}

// ---------- 登录失败退避 ----------

func TestAttemptLimiterBackoff(t *testing.T) {
	l := newAttemptLimiter()
	const key = "login:1.2.3.4"

	// 前两次失败不罚：手滑输错很正常，本人不该被关在门外
	for i := 1; i <= 2; i++ {
		if ok, _ := l.allow(key); !ok {
			t.Fatalf("第 %d 次失败后不该被拦（前两次免罚）", i)
		}
		l.fail(key)
	}
	// 第三次失败开始退避
	l.fail(key)
	ok, wait := l.allow(key)
	if ok {
		t.Fatal("连续三次失败后应当被拦住")
	}
	if wait <= 0 || wait > 5*time.Second {
		t.Errorf("第三次失败的等待时间应当是个小值（3 秒量级），实际 %v", wait)
	}

	// 成功之后清零，不该继续被罚
	l.reset(key)
	if ok, _ := l.allow(key); !ok {
		t.Error("登录成功（reset）之后应当立刻放行")
	}
}

func TestAttemptLimiterIsolatesKeys(t *testing.T) {
	l := newAttemptLimiter()
	for i := 0; i < 6; i++ {
		l.fail("login:10.0.0.1")
	}
	if ok, _ := l.allow("login:10.0.0.1"); ok {
		t.Error("被爆破的那个 IP 应当被拦住")
	}
	if ok, _ := l.allow("login:10.0.0.2"); !ok {
		t.Error("退避要按来源分开 —— 一个 IP 被爆破不该连累其他人")
	}
}

func TestClientIPIgnoresForwardedHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.168.1.9:54321"
	// X-Forwarded-For 谁都能伪造，拿它做限流等于没限
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(r); got != "192.168.1.9" {
		t.Errorf("只认 RemoteAddr，实际取了 %s", got)
	}
	r.RemoteAddr = "没有端口"
	if got := clientIP(r); got != "没有端口" {
		t.Errorf("解析失败时应当原样返回，实际 %s", got)
	}
}

// ---------- 同源校验（CSRF）----------

func TestSameOriginRequest(t *testing.T) {
	newReq := func(method, origin, referer string) *http.Request {
		r := httptest.NewRequest(method, "http://127.0.0.1:8097/api/config", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}

	cases := []struct {
		name    string
		req     *http.Request
		wantOK  bool
		comment string
	}{
		{"GET 不校验（图片、静态资源不可能是 CSRF 跳板）", newReq(http.MethodGet, "http://evil.com", ""), true, ""},
		{"同源 POST 放行", newReq(http.MethodPost, "http://127.0.0.1:8097", ""), true, ""},
		{"端口不同就是不同源", newReq(http.MethodPost, "http://127.0.0.1:9999", ""), false, ""},
		{"跨站 POST 拒绝", newReq(http.MethodPost, "http://evil.com", ""), false, ""},
		{"Origin 缺失时看 Referer", newReq(http.MethodPost, "", "http://127.0.0.1:8097/index.html"), true, ""},
		{"Referer 是外站则拒绝", newReq(http.MethodPost, "", "http://evil.com/x"), false, ""},
		{"两者都没有（curl / 脚本）放行", newReq(http.MethodPost, "", ""), true, "非浏览器客户端本来就不受「自动带 cookie」影响"},
		{"大小写不敏感", newReq(http.MethodPost, "http://127.0.0.1:8097", "HTTP://127.0.0.1:8097/"), true, ""},
	}
	for _, c := range cases {
		if got := sameOriginRequest(c.req); got != c.wantOK {
			t.Errorf("%s：期望 %v，实际 %v %s", c.name, c.wantOK, got, c.comment)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	setSecurityHeaders(w)
	h := w.Header()
	for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if h.Get(k) == "" {
			t.Errorf("缺少安全响应头 %s", k)
		}
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "object-src 'none'") {
		t.Errorf("CSP 应当封死 iframe 嵌套与插件：%s", csp)
	}
	// 图片全部走本机代理，img-src 不该放开到任意站点
	if strings.Contains(csp, "img-src *") {
		t.Errorf("img-src 不该是通配：%s", csp)
	}
}

// ---------- 脱敏 ----------

func TestStorePublicMasksSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) {
		c.Password = "emby-password"
		c.APIKey = "emby-api-key"
		c.Token = "emby-token"
		c.MetaTubeToken = "metatube-token"
		c.JavBusCookie = "javbus-cookie"
		c.OpenAI.APIKey = "sk-openai"
		c.Username = "emby-user"
	}); err != nil {
		t.Fatal(err)
	}

	pub := store.Public()
	if pub.Password != "" || pub.APIKey != "" || pub.Token != "" ||
		pub.MetaTubeToken != "" || pub.JavBusCookie != "" || pub.OpenAI.APIKey != "" {
		t.Errorf("敏感字段必须一律置空，实际：%+v / %+v", pub.Config, pub.OpenAI)
	}
	if pub.Config.Auth.PasswordHash != "" {
		t.Error("密码哈希也不该下发（少一次离线爆破的机会）")
	}
	if !pub.LoggedIn {
		t.Error("logged_in 要根据抹掉之前的令牌算出来的")
	}
	for name, got := range map[string]bool{
		"emby_password":  pub.Secrets.EmbyPassword,
		"emby_api_key":   pub.Secrets.EmbyAPIKey,
		"emby_token":     pub.Secrets.EmbyToken,
		"metatube_token": pub.Secrets.MetaTubeToken,
		"javbus_cookie":  pub.Secrets.JavBusCookie,
		"openai_api_key": pub.Secrets.OpenAIAPIKey,
	} {
		if !got {
			t.Errorf("secrets.%s 应当为 true（前端据此提示「已保存」）", name)
		}
	}
	// 非敏感字段照旧下发
	if pub.Username != "emby-user" {
		t.Errorf("非敏感字段不该被抹掉：%q", pub.Username)
	}
	if pub.EmbyURL == "" || pub.ConfigPath != path {
		t.Errorf("地址与路径要照常给前端：%q / %q", pub.EmbyURL, pub.ConfigPath)
	}

	// 脱敏只是副本，不能把真实配置改坏
	raw := store.Get()
	if raw.Password != "emby-password" || raw.Token != "emby-token" || raw.APIKey != "emby-api-key" {
		t.Error("Public() 不能动到真实配置")
	}
}

// ---------- 配置默认值 ----------

func TestAuthUsernameDefaults(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().Auth.Username; got != DefaultAuthUsername {
		t.Errorf("没配用户名时应当是 %q，实际 %q", DefaultAuthUsername, got)
	}
	if store.Get().Auth.PasswordHash != "" {
		t.Error("默认不该带任何密码哈希（第一次启动由 BootstrapAuth 生成）")
	}
}

// 环境变量是唯一的找回入口，优先级必须高于配置里已有的哈希。
func TestBootstrapAuthEnvWins(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(store, nil)

	generated := app.BootstrapAuth()
	if len(generated) != authPwdLen {
		t.Fatalf("首次启动应当生成并返回随机密码，实际 %q", generated)
	}
	first := store.Get().Auth.PasswordHash
	if first == "" || !verifyPassword(first, generated) {
		t.Fatal("生成的哈希与自己返回的密码对不上")
	}
	if !store.Get().Auth.Generated {
		t.Error("自动生成的密码要打上 generated 标记，界面上才好提示「建议改掉」")
	}
	// 第二次启动：密码已经在了，不再生成也不该打印
	if pw := app.BootstrapAuth(); pw != "" {
		t.Errorf("已有密码时不该再生成：%q", pw)
	}
	if store.Get().Auth.PasswordHash != first {
		t.Error("已有密码时不该被覆盖")
	}

	// 环境变量覆盖
	t.Setenv("EMBYME_AUTH_PASSWORD", "from-env-pass")
	t.Setenv("EMBYME_AUTH_USER", "boss")
	if pw := app.BootstrapAuth(); pw != "" {
		t.Error("密码来自环境变量时不该再往控制台打印一遍")
	}
	cfg := store.Get()
	if cfg.Auth.Username != "boss" {
		t.Errorf("用户名应当被环境变量覆盖，实际 %q", cfg.Auth.Username)
	}
	if !verifyPassword(cfg.Auth.PasswordHash, "from-env-pass") {
		t.Error("环境变量里的密码应当生效")
	}
	if verifyPassword(cfg.Auth.PasswordHash, "old-password") {
		t.Error("旧密码应当失效")
	}
	if cfg.Auth.Generated {
		t.Error("环境变量指定的密码不算「自动生成的」")
	}
}

func TestAuthConfigSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("persisted")
	if err := store.Update(func(c *Config) {
		c.Auth.Username = "someone"
		c.Auth.PasswordHash = hash
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := reloaded.Get()
	if cfg.Auth.Username != "someone" || !verifyPassword(cfg.Auth.PasswordHash, "persisted") {
		t.Errorf("重启后认证信息应当还在：%+v", cfg.Auth)
	}
	// 配置文件权限：里面虽然只有派生值，也没必要给同机其他用户读。
	// Windows 上 Go 只能表示「只读/可写」，0600 反映不出来（NTFS 走的是 ACL），
	// 所以这条只在类 Unix 上较真。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("config.json 不应当对同组 / 其他人可读：%v", info.Mode().Perm())
		}
	}
}
