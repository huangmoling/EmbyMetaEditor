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
}

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

// publicConfig 是返回给前端的结构（隐藏内部 token 细节可保留，此处本地工具直接透出便于编辑）。
type publicConfig struct {
	Config
	ConfigPath  string `json:"config_path"`
	LoggedIn    bool   `json:"logged_in"`
	DataDirPath string `json:"data_dir"`
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
