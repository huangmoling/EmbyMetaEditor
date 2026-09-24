package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 为什么要有这一层：
//
// 这个界面能读到 Emby 的 API Key / 令牌，能改媒体库元数据、能上传图片。
// 本机双击运行时只监听 127.0.0.1，谁来都无所谓；但 Docker / NAS 部署时
// 端口一旦映射出去（`-p 8097:8097`，没有 127.0.0.1: 前缀），
// 整个界面就等于对局域网（甚至公网）裸奔 —— 谁打开谁就是管理员。
//
// 所以加一道**进程自己的**登录：与 Emby 登录无关，保护的是「谁能打开这个界面」。
// 下面这些是刻意选的取舍：
//
//   - 密码只存 PBKDF2-SHA256 派生值（200k 轮 + 16 字节随机盐），不存明文，
//     比对走 `subtle.ConstantTimeCompare`，避免时序侧信道。
//   - 会话是**内存**里的随机令牌（32 字节），不进磁盘 —— 进程重启即失效，
//     卷里那份 config.json 被拿走也换不来一个可用会话。
//   - Cookie 带 HttpOnly + SameSite=Lax，JS 拿不到；不加 Secure 是因为
//     容器里默认是明文 HTTP，加了反而整个登录流程失效
//     （要上公网请在前面套一层 HTTPS 反代，见 README）。
//   - 写请求校验 Origin / Referer，配合「只收 JSON」挡住表单型 CSRF。
//   - 登录失败按来源 IP 退避，挡住暴力破解。
//
// 密码从哪来，按优先级：
//   1. 环境变量 EMBYME_AUTH_PASSWORD（每次启动都会重新派生并覆盖，是**找回入口**）
//   2. config.json 里的 auth.password_hash
//   3. 都没有 → 自动生成一个随机密码，启动时在控制台打印出来
//      （宁可让用户去日志里找一次密码，也不能默认放一个没有密码的管理后台出来）

const (
	authAlgo       = "pbkdf2-sha256"
	authIterations = 200000
	authSaltLen    = 16
	authKeyLen     = 32

	// sessionCookie 是会话 cookie 名。带 embyme_ 前缀免得和 Emby 自己的 cookie 撞。
	sessionCookie = "embyme_session"
	sessionTTL    = 7 * 24 * time.Hour

	// 自动生成的初始密码长度。用去掉了易混字符的字母表（没有 0/O/l/1/I）。
	authPwdLen = 14
)

// authPwdAlphabet 去掉了 0 O 1 l I 这些看起来一样的字符，方便用户从日志里抄。
const authPwdAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// ---------- 密码派生 ----------

// hashPassword 生成 `pbkdf2-sha256$<轮数>$<盐>$<派生值>` 形式的可存储字符串。
// 轮数与盐都写在串里，以后调参不会让老密码失效。
func hashPassword(password string) (string, error) {
	salt := make([]byte, authSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, authIterations, authKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", authAlgo, authIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword 校验明文密码。任何格式问题都只返回 false（不区分「密码错」和
// 「哈希坏了」），避免把内部状态透给未登录的人。
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != authAlgo {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 5000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) < 16 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// randomPassword 用 crypto/rand 生成随机密码；取模偏差用拒绝采样消掉。
func randomPassword(n int) string {
	out := make([]byte, 0, n)
	limit := 256 - (256 % len(authPwdAlphabet))
	buf := make([]byte, 1)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			// 随机源都取不到的话，退回 hex（不引入弱密码，只是可读性差一点）
			return randHex(n)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, authPwdAlphabet[int(buf[0])%len(authPwdAlphabet)])
	}
	return string(out)
}

// ---------- 会话 ----------

type session struct {
	user    string
	expires time.Time
}

// sessions 是内存会话表。进程重启 = 全部登出，这是有意的：
// 会话令牌不落盘，config.json 泄漏也换不到一个可用会话。
type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}} }

// issue 新建会话并返回令牌。
func (s *sessions) issue(user string) string {
	tok := randHex(32) // 256 bit
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.m[tok] = &session{user: user, expires: time.Now().Add(sessionTTL)}
	return tok
}

// valid 判断令牌是否对应一个未过期的会话（顺带续期，活跃用户不会被中途踢掉）。
func (s *sessions) valid(tok string) (*session, bool) {
	if tok == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	sess, ok := s.m[tok]
	if !ok || time.Now().After(sess.expires) {
		delete(s.m, tok)
		return nil, false
	}
	sess.expires = time.Now().Add(sessionTTL)
	return sess, true
}

func (s *sessions) drop(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tok)
}

// dropOthers 保留当前会话，踢掉其余的 —— 改密码之后用。
func (s *sessions) dropOthers(keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok := range s.m {
		if tok != keep {
			delete(s.m, tok)
		}
	}
}

func (s *sessions) gcLocked() {
	now := time.Now()
	for tok, sess := range s.m {
		if now.After(sess.expires) {
			delete(s.m, tok)
		}
	}
}

// ---------- 登录失败退避 ----------

// 连续失败第 n 次后要等多久。前两次不罚（手滑输错很正常），
// 之后指数上升，封顶 15 分钟 —— 在线爆破撞不动，本人输错两次也不会被关门外。
var authBackoff = []time.Duration{
	0, 0, 3 * time.Second, 10 * time.Second, 30 * time.Second,
	2 * time.Minute, 5 * time.Minute, 15 * time.Minute,
}

type attemptState struct {
	fails int
	until time.Time
	last  time.Time
}

type attemptLimiter struct {
	mu sync.Mutex
	m  map[string]*attemptState
}

func newAttemptLimiter() *attemptLimiter { return &attemptLimiter{m: map[string]*attemptState{}} }

// allow 返回是否放行；被挡时同时给出还要等多久。
func (l *attemptLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked()
	st, ok := l.m[key]
	if !ok {
		return true, 0
	}
	if wait := time.Until(st.until); wait > 0 {
		return false, wait
	}
	return true, 0
}

func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.m[key]
	if !ok {
		st = &attemptState{}
		l.m[key] = st
	}
	st.fails++
	st.last = time.Now()
	// 前两次失败不罚：idx = fails-1，所以 fails=1/2 都落在前两个 0 上。
	idx := st.fails - 1
	if idx >= len(authBackoff) {
		idx = len(authBackoff) - 1
	}
	st.until = time.Now().Add(authBackoff[idx])
}

func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

func (l *attemptLimiter) gcLocked() {
	now := time.Now()
	for k, st := range l.m {
		if now.Sub(st.last) > time.Hour && now.After(st.until) {
			delete(l.m, k)
		}
	}
}

// clientIP 取来源 IP。**只看 RemoteAddr，不认 X-Forwarded-For**：
// 那个头谁都能伪造，拿它做限流等于没限（伪造一个 IP 就绕过了）。
// 代价是套了反代时所有请求共用一个桶 —— 对单人使用的工具来说可以接受。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- 中间件 ----------

// publicAPIPaths 是未登录也能访问的接口。
//
// 静态资源（/、/app.js、/style.css、/favicon.png）一律放行：
// 登录界面本身就活在这些文件里，而它们不含任何凭据 ——
// 真正的数据全在 /api/ 下面，那才是要拦的。
var publicAPIPaths = map[string]bool{
	"/api/auth/status": true,
	"/api/auth/login":  true,
	"/api/auth/logout": true, // 放行是为了「会话过期也能清掉 cookie」
}

// guard 把安全响应头、CSRF 校验、登录校验串在路由前面。
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if !sameOriginRequest(r) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("跨站请求已被拒绝"))
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/") && !publicAPIPaths[r.URL.Path] {
			if _, ok := a.currentSession(r); !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"ok": false, "error": "登录状态已失效，请重新登录", "code": "unauthorized",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// setSecurityHeaders 给每个响应挂上基础防护头。
//
// 关于 CSP：界面里有内联 style 和内联 onclick，所以 script/style 必须带
// 'unsafe-inline'，否则整个界面直接白屏 —— 这一条挡不住 XSS，但把
// 「加载外部脚本 / 被 iframe 套走」这两类封死了，配合 X-Frame-Options 够用。
// 图片全部走本机 /api/img 与 /api/emby/image（服务端代取），所以 img-src 收得很紧。
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data: blob:; connect-src 'self'; font-src 'self' data:; "+
			"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
}

// sameOriginRequest 校验写请求的来源，挡 CSRF。
//
// 只对非安全方法生效；GET/HEAD 不做要求（图片、静态资源不可能是 CSRF 跳板）。
// Origin / Referer **都没有**时放行：那是 curl、脚本这类非浏览器客户端，
// 它们本来就不受「浏览器自动带 cookie」的影响。
// 另外所有写接口都只接受 JSON body，HTML 表单伪造不出 application/json，
// 这是第二道闸。
func sameOriginRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ---------- 认证状态 ----------

// currentSession 从 cookie 里解析当前会话。
func (a *App) currentSession(r *http.Request) (*session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	return a.sess.valid(c.Value)
}

func setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,                 // JS 读不到，降低 XSS 后的收益
		SameSite: http.SameSiteLaxMode, // 跨站表单提交不带 cookie
		MaxAge:   maxAge,
	})
}

// ---------- 启动引导 ----------

// applyAuthEnv 处理环境变量的密码覆盖，返回环境变量是否真的提供了密码。
//
// 这是**唯一的找回入口**：忘了密码、或者想换密码又进不去界面时，
// 起容器时带上 `-e EMBYME_AUTH_PASSWORD=新密码` 即可（每次启动都会重新派生覆盖）。
func (a *App) applyAuthEnv() bool {
	pw := os.Getenv("EMBYME_AUTH_PASSWORD")
	if strings.TrimSpace(pw) == "" {
		return false
	}
	user := strings.TrimSpace(os.Getenv("EMBYME_AUTH_USER"))
	if user == "" {
		user = a.store.Get().Auth.Username
	}
	hash, err := hashPassword(pw)
	if err != nil {
		fmt.Printf("  [警告] EMBYME_AUTH_PASSWORD 无法派生，沿用原密码：%v\n", err)
		return false
	}
	_ = a.store.Update(func(c *Config) {
		c.Auth.Username = user
		c.Auth.PasswordHash = hash
		c.Auth.Generated = false
	})
	return true
}

// BootstrapAuth 在启动时准备好认证配置，返回**需要打印给用户的初始密码**（没有则空串）。
//
// 优先级：环境变量 > 已保存的哈希 > 现场生成一个随机密码。
// 最后那种情况必须把密码打到控制台（Docker 里就是 `docker logs`），
// 否则用户根本拿不到密码，只能删卷重来。
func (a *App) BootstrapAuth() string {
	if a.applyAuthEnv() {
		return "" // 密码来自环境变量，用户自己知道，不该再往日志里写一遍
	}
	if a.store.Get().Auth.PasswordHash != "" {
		return ""
	}
	pw := randomPassword(authPwdLen)
	hash, err := hashPassword(pw)
	if err != nil {
		fmt.Printf("  [警告] 初始密码生成失败，将无法登录：%v\n", err)
		return ""
	}
	_ = a.store.Update(func(c *Config) {
		if strings.TrimSpace(c.Auth.Username) == "" {
			c.Auth.Username = DefaultAuthUsername
		}
		c.Auth.PasswordHash = hash
		c.Auth.Generated = true
	})
	return pw
}

// ---------- 认证接口 ----------

// handleAuthStatus 告诉前端「要不要登录、现在登没登」。
// 这是登出状态下唯一会说话的状态接口，所以只回最少的字段。
func (a *App) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	_, ok := a.currentSession(r)
	writeOK(w, map[string]any{
		"required":        true,
		"authenticated":   ok,
		"username":        cfg.Auth.Username,
		"password_is_new": cfg.Auth.Generated, // 还是首次自动生成的初始密码，建议改掉
		"session_hours":   int(sessionTTL.Hours()),
	})
}

func (a *App) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ip := clientIP(r)
	if ok, wait := a.limit.allow("login:" + ip); !ok {
		secs := int(wait.Seconds()) + 1
		writeErr(w, http.StatusTooManyRequests,
			fmt.Errorf("登录失败次数过多，请等待 %d 秒后再试", secs))
		return
	}

	cfg := a.store.Get()
	user := strings.TrimSpace(in.Username)
	// 用户名和密码任一不对都回同一句话：不告诉对方「用户名是对的、只是密码错」。
	// 密码校验放在用户名之后，但两边都会走完（用 || 短路会泄露用户名是否存在，
	// 这里密码是主判据，所以先比密码再比用户名反而更慢——直接顺序比，代价可接受）。
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.Auth.Username)) == 1
	passOK := verifyPassword(cfg.Auth.PasswordHash, in.Password)

	if !userOK || !passOK {
		a.limit.fail("login:" + ip)
		fmt.Printf("  [认证] 登录失败（来源 %s）\n", ip)
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("用户名或密码不正确"))
		return
	}

	a.limit.reset("login:" + ip)
	tok := a.sess.issue(cfg.Auth.Username)
	setSessionCookie(w, tok, int(sessionTTL.Seconds()))
	fmt.Printf("  [认证] 登录成功（来源 %s）\n", ip)
	writeOK(w, map[string]any{"username": cfg.Auth.Username})
}

// handleAuthLogout 注销当前会话。公开访问（会话过期时也要能清 cookie）。
func (a *App) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sess.drop(c.Value)
	}
	setSessionCookie(w, "", -1)
	writeOK(w, true)
}

// handleAuthPassword 修改访问用户名 / 密码。需要已登录。
//
// 改完把**其他**会话全部踢掉（当前这个留着，否则用户改完密码自己也被踢下线），
// 这样「密码可能已泄漏」的情况下改一次就能收口。
func (a *App) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OldPassword string `json:"old_password"`
		Username    string `json:"username"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg := a.store.Get()

	// 改密码接口同样要退避，否则它就成了「不限额试旧密码」的旁路。
	if ok, wait := a.limit.allow("password:" + clientIP(r)); !ok {
		writeErr(w, http.StatusTooManyRequests,
			fmt.Errorf("失败次数过多，请等待 %d 秒后再试", int(wait.Seconds())+1))
		return
	}

	// 改密码必须验旧密码（拿到别人没锁屏的浏览器也不能直接换掉密码）。
	if !verifyPassword(cfg.Auth.PasswordHash, in.OldPassword) {
		a.limit.fail("password:" + clientIP(r))
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("当前密码不正确"))
		return
	}
	a.limit.reset("password:" + clientIP(r))

	newUser := strings.TrimSpace(in.Username)
	if newUser == "" {
		newUser = cfg.Auth.Username
	}
	if len([]rune(newUser)) > 64 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("用户名过长"))
		return
	}
	pwd := in.NewPassword
	if pwd != "" {
		if len(pwd) < 8 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("新密码至少 8 位"))
			return
		}
		if len(pwd) > 256 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("新密码过长"))
			return
		}
	}
	hash := cfg.Auth.PasswordHash // 新密码留空 = 只改用户名，密码不动
	if pwd != "" {
		h, err := hashPassword(pwd)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		hash = h
	}
	if err := a.store.Update(func(c *Config) {
		c.Auth.Username = newUser
		c.Auth.PasswordHash = hash
		c.Auth.Generated = false // 用户自己设过了
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sess.dropOthers(c.Value)
	}
	writeOK(w, map[string]any{"username": newUser})
}
