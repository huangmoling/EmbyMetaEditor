package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config 是所有可配置项的集合，持久化到程序目录下的 config.json。
type Config struct {
	// ---- Emby ----
	EmbyURL  string `json:"emby_url"`
	Username string `json:"username"`
	Password string `json:"password"`
	APIKey   string `json:"api_key"`
	Token    string `json:"token"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	DeviceID string `json:"device_id"`

	// ---- MetaTube ----
	MetaTubeURL   string `json:"metatube_url"`
	MetaTubeToken string `json:"metatube_token"`

	// ---- gfriends 头像库 ----
	GfriendsTreeURL string `json:"gfriends_tree_url"`
	GfriendsCDN     string `json:"gfriends_cdn"`

	// ---- javbus ----
	JavBusURL    string `json:"javbus_url"`
	JavBusCookie string `json:"javbus_cookie"`

	// ---- 国产传媒专项刮削 ----
	// 键为站点标识（xchina / madouqu / madou / 7mmtv），值为站点根地址。
	// 留空则用 defaultCNSites() 里的默认值，方便换镜像域名。
	CNSites map[string]string `json:"cn_sites"`

	// ---- OpenAI / 翻译 ----
	// 刮削番号时把非中文的标题、简介翻成中文。base_url 填官方
	// https://api.openai.com/v1 或任意 OpenAI 兼容的中转（如 api.gptgod.online/v1）。
	// Enabled 是总开关；即使配了 key，关掉就不翻译。
	OpenAI OpenAIConfig `json:"openai"`

	// ---- 通用 ----
	Proxy           string `json:"proxy"`
	InsecureTLS     bool   `json:"insecure_tls"`
	AutoRefresh     bool   `json:"auto_refresh"`
	OverwriteImages bool   `json:"overwrite_images"`
	Concurrency     int    `json:"concurrency"`
	JavBusInterval  int    `json:"javbus_interval_ms"`

	// ---- 界面访问认证 ----
	// 保护的是「谁能打开这个界面」，和上面的 Emby 登录完全是两回事。
	Auth AuthConfig `json:"auth"`
}

// AuthConfig 是界面自己的登录凭据。
//
// 只存派生的密码哈希（见 auth.go 的 hashPassword），**不存明文**：
// config.json 会被备份、会被挂进卷里、偶尔还会被人贴出来求助，
// 明文密码进去就等于泄漏。
//
// 密码从哪来见 auth.go 顶部的说明（环境变量 > 配置 > 首次自动生成）。
type AuthConfig struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	// Generated 表示当前用的还是首次启动自动生成的那个密码，
	// 界面上会提示「建议改掉」，改过之后置 false。
	Generated bool `json:"password_generated"`
}

// DefaultAuthUsername 是没配置用户名时的默认值。
const DefaultAuthUsername = "admin"

// OpenAIConfig 是翻译功能的配置。
//
// 兼容官方与各类中转：base_url 只填到「接口根」即可（如 https://api.openai.com/v1
// 或 https://api.gptgod.online/v1），代码会自动拼上 /chat/completions。
// 兼容中转经常换域名、改路径，所以这里不强制约定具体厂商。
type OpenAIConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	Enabled bool   `json:"enabled"` // 翻译总开关
}

// Ready 表示翻译可以真正发起（开关开 + 地址和 key 都在）。
func (o OpenAIConfig) Ready() bool {
	return o.Enabled && strings.TrimSpace(o.BaseURL) != "" && strings.TrimSpace(o.APIKey) != ""
}

// DefaultConfig 返回带默认值的配置。
func DefaultConfig() Config {
	return Config{
		EmbyURL:         "http://127.0.0.1:8096",
		MetaTubeURL:     "http://127.0.0.1:8080",
		GfriendsTreeURL: "https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/Filetree.json",
		GfriendsCDN:     "https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/",
		JavBusURL:       "https://www.javbus.com",
		JavBusCookie:    "age=verified; dv=1; existmag=mag",
		AutoRefresh:     true,
		Concurrency:     4,
		JavBusInterval:  1500,
	}
}

// normalize 补齐空字段，避免老配置缺项导致行为异常。
func (c *Config) normalize() {
	d := DefaultConfig()
	if strings.TrimSpace(c.EmbyURL) == "" {
		c.EmbyURL = d.EmbyURL
	}
	if strings.TrimSpace(c.MetaTubeURL) == "" {
		c.MetaTubeURL = d.MetaTubeURL
	}
	if strings.TrimSpace(c.GfriendsTreeURL) == "" {
		c.GfriendsTreeURL = d.GfriendsTreeURL
	}
	if strings.TrimSpace(c.GfriendsCDN) == "" {
		c.GfriendsCDN = d.GfriendsCDN
	}
	if strings.TrimSpace(c.JavBusURL) == "" {
		c.JavBusURL = d.JavBusURL
	}
	if strings.TrimSpace(c.JavBusCookie) == "" {
		c.JavBusCookie = d.JavBusCookie
	}
	if c.CNSites == nil {
		c.CNSites = map[string]string{}
	}
	for k, v := range defaultCNSites() {
		if strings.TrimSpace(c.CNSites[k]) == "" {
			c.CNSites[k] = v
		}
		c.CNSites[k] = strings.TrimRight(strings.TrimSpace(c.CNSites[k]), "/")
	}
	if c.Concurrency <= 0 || c.Concurrency > 32 {
		c.Concurrency = d.Concurrency
	}
	if c.OpenAI.Model == "" {
		c.OpenAI.Model = "gpt-4o-mini"
	}
	c.OpenAI.BaseURL = strings.TrimRight(strings.TrimSpace(c.OpenAI.BaseURL), "/")
	if c.JavBusInterval <= 0 {
		c.JavBusInterval = d.JavBusInterval
	}
	if c.DeviceID == "" {
		c.DeviceID = randHex(8)
	}
	if strings.TrimSpace(c.Auth.Username) == "" {
		c.Auth.Username = DefaultAuthUsername
	}
	c.EmbyURL = strings.TrimRight(strings.TrimSpace(c.EmbyURL), "/")
	c.MetaTubeURL = strings.TrimRight(strings.TrimSpace(c.MetaTubeURL), "/")
	c.JavBusURL = strings.TrimRight(strings.TrimSpace(c.JavBusURL), "/")
	if !strings.HasSuffix(c.GfriendsCDN, "/") {
		c.GfriendsCDN += "/"
	}
}

// Store 提供并发安全的配置读写。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// NewStore 从 path 加载配置，文件不存在则使用默认值。
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, cfg: DefaultConfig()}
	raw, err := os.ReadFile(path)
	if err == nil {
		var c Config
		if e := json.Unmarshal(raw, &c); e == nil {
			s.cfg = c
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	s.cfg.normalize()
	return s, nil
}

// Get 返回配置副本。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 用 fn 修改配置并落盘。
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	fn(&s.cfg)
	s.cfg.normalize()
	s.mu.Unlock()
	return s.save()
}

// save 写盘（调用方需已持锁或接受竞态）。
func (s *Store) save() error {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// publicConfig 是返回给前端的配置。
//
// **敏感字段一律置空**：Emby 账号密码、API Key、令牌、MetaTube token、
// javbus cookie、OpenAI key —— 它们每一个都等于**另一个系统**的权限，
// 没有任何理由跟着页面进浏览器内存（何况还有浏览器插件、截图、复制粘贴这些外溢口）。
// 前端拿不到值，只能通过 Secrets 里的一组布尔值显示「已保存，留空则不修改」。
//
// 挡住未登录访问的是 guard 中间件，这里做的是第二层：即使登录了也不下发。
type publicConfig struct {
	Config
	ConfigPath  string      `json:"config_path"`
	LoggedIn    bool        `json:"logged_in"`
	DataDirPath string      `json:"data_dir"`
	Version     string      `json:"version"`
	RepoURL     string      `json:"repo_url"`
	Secrets     secretFlags `json:"secrets"`
	Auth        authPublic  `json:"auth"`
}

// secretFlags 说明哪些敏感项已经存过了（值本身不下发）。
type secretFlags struct {
	EmbyPassword  bool `json:"emby_password"`
	EmbyAPIKey    bool `json:"emby_api_key"`
	EmbyToken     bool `json:"emby_token"`
	MetaTubeToken bool `json:"metatube_token"`
	JavBusCookie  bool `json:"javbus_cookie"`
	OpenAIAPIKey  bool `json:"openai_api_key"`
}

// authPublic 是界面自己能看到的认证信息，同样不含哈希。
type authPublic struct {
	Username  string `json:"username"`
	Generated bool   `json:"password_generated"`
}

// Public 返回脱敏后的配置副本，供 /api/config 使用。
func (s *Store) Public() publicConfig {
	c := s.Get()
	cfg := publicConfig{
		LoggedIn:    c.Token != "",
		ConfigPath:  s.Path(),
		DataDirPath: dataDir(),
		Version:     appVersion,
		RepoURL:     repoURL,
		Secrets: secretFlags{
			EmbyPassword:  c.Password != "",
			EmbyAPIKey:    c.APIKey != "",
			EmbyToken:     c.Token != "",
			MetaTubeToken: c.MetaTubeToken != "",
			JavBusCookie:  c.JavBusCookie != "",
			OpenAIAPIKey:  c.OpenAI.APIKey != "",
		},
		Auth: authPublic{Username: c.Auth.Username, Generated: c.Auth.Generated},
	}

	c.Password = ""
	c.APIKey = ""
	c.Token = ""
	c.MetaTubeToken = ""
	c.JavBusCookie = ""
	c.OpenAI.APIKey = ""
	// 密码哈希虽然是派生的，也没必要出门 —— 不下发就少一次离线爆破的机会。
	c.Auth.PasswordHash = ""
	cfg.Config = c
	return cfg
}

// dataDir 决定配置与缓存存放目录。
func dataDir() string {
	if v := strings.TrimSpace(os.Getenv("EMBYME_HOME")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		// go run 会把可执行文件放到临时目录，这时改用当前工作目录
		if !strings.Contains(strings.ToLower(d), "go-build") {
			return d
		}
	}
	wd, _ := os.Getwd()
	return wd
}
