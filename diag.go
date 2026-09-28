package main

// 诊断包导出：把排障需要的东西打成一个 zip。
//
// 为什么要做：这个工具的问题几乎都出在「本机连不上对方」——
// Emby 的地址/权限、MetaTube 有没有起、javbus 是不是被 Cloudflare 拦了、
// 代理是不是劫持了 127.0.0.1。这些在用户的描述里全是「就是不行」，
// 而真正有用的信息（配置长什么样、任务日志里那句 "HTTP 403"、缓存里有没有索引）
// 只有用户机器上才有。让用户手动去翻 `cache/` 和 `config.json` 不现实 ——
// 那里面还有密钥，谁也不敢直接贴出来。
//
// 所以这里做两件事：**脱敏** + **打包**。脱敏用「键名模式匹配」而不是逐个字段列举 ——
// 后者在以后加了新密钥字段时会被悄悄漏掉，而这是不可接受的失败方式。
//
// ⚠️ 这个包会被用户贴到公开的 issue 里。改动这里的任何东西时，
// `TestDiagBundleRedactsSecrets` 会盯着「配过的密钥不许出现在包里」。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// diagStart 近似进程启动时间（包初始化时记下），用来算运行时长。
var diagStart = time.Now()

// diagSecretKeyPatterns 是「键名里出现就当作密钥」的模式。
//
// 用模式匹配而不是列举字段名，是为了让**以后新加的密钥字段自动被脱敏**。
// 漏掉一个密钥的代价（用户把 token 贴到公开 issue 里）远大于
// 多脱敏一个无害字段的代价。
var diagSecretKeyPatterns = []string{
	"password", "passwd", "api_key", "apikey", "token", "cookie",
	"secret", "hash", "credential", "authorization",
}

func diagIsSecretKey(k string) bool {
	lk := strings.ToLower(k)
	for _, p := range diagSecretKeyPatterns {
		if strings.Contains(lk, p) {
			return true
		}
	}
	return false
}

// diagRedactMark 说明「这里原本有值，但被脱敏了」，并给出长度。
//
// 长度不是敏感信息（不泄露内容），但对排障有用：能看出「密钥填了但只有 3 个字符」
// 这种典型的手抖。
func diagRedactMark(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			return ""
		}
		return "<已脱敏>"
	}
	if s == "" {
		return "" // 保持「没设置」可区分
	}
	return fmt.Sprintf("<已脱敏：%d 字符>", len(s))
}

// scrubURLCredentials 去掉 URL 里的用户名密码（`http://user:pass@host`）。
// 代理地址和 Emby 地址都可能这么写。
func scrubURLCredentials(raw string) string {
	if !strings.Contains(raw, "@") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("<已脱敏>")
	return u.String()
}

// redactConfigValue 递归脱敏任意 JSON 结构。
func redactConfigValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			// 只有**非空字符串**才可能是密钥内容。`password_generated` 这种
			// 键名带 password 但值是 bool 的开关，脱掉它反而丢掉了排障信息
			// （看不出「密码还是自动生成的那一个」）。
			if diagIsSecretKey(k) {
				if _, isStr := vv.(string); isStr || vv == nil {
					out[k] = diagRedactMark(vv)
					continue
				}
			}
			out[k] = redactConfigValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = redactConfigValue(vv)
		}
		return out
	case string:
		// URL 类字段顺手去掉 userinfo
		if strings.Contains(t, "://") {
			return scrubURLCredentials(t)
		}
		return t
	default:
		return v
	}
}

// redactedConfigMap 把配置转成可以安全贴出去的 map。
func redactedConfigMap(c Config) map[string]any {
	raw, err := json.Marshal(c)
	if err != nil {
		return map[string]any{"error": "配置序列化失败：" + err.Error()}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"error": "配置解析失败：" + err.Error()}
	}
	out, _ := redactConfigValue(m).(map[string]any)
	return out
}

// diagInput 是打包需要的全部原料。做成结构体是为了让测试可以直接构造，
// 不用起 HTTP 服务、也不用碰真实的 config。
type diagInput struct {
	Version string
	Config  Config
	// ConfigPath 由 store 提供 —— Config 本身不含路径（那是 publicConfig 的事）。
	ConfigPath string
	History    []SyncRecord
	Jobs       []map[string]any
	Cache      []diagCacheFile
}

type diagCacheFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// buildDiagZip 生成诊断包。
func buildDiagZip(in diagInput) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	write := func(name string, body []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}
	writeJSON := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return write(name, append(b, '\n'))
	}

	// ---- README：先说清这个包里有什么、被脱敏了什么 ----
	readme := []string{
		"Emby 元数据编辑器 · 诊断包",
		"",
		"生成时间：" + time.Now().Format(time.RFC3339),
		"",
		"包内文件：",
		"  info.txt              版本 / 运行环境 / 运行时长",
		"  config.redacted.json  当前配置（**所有密钥已脱敏**）",
		"  sync_history.json     写入历史（不含快照本体，只有元信息）",
		"  jobs.json             最近的后台任务与它们的日志",
		"  cache.json            数据目录里缓存/落盘文件的清单与大小",
		"",
		"关于脱敏：凡是键名里带 password / token / cookie / api_key / secret / hash",
		"的字段，值都会被替换成 `<已脱敏：N 字符>`（N 是原值长度）。",
		"URL 里的 user:pass@ 也会被抹掉。",
		"",
		"如果发现包里仍有不该出现的内容，请只截取相关片段而不是整包公开。",
		"",
	}
	if err := write("README.txt", []byte(strings.Join(readme, "\n"))); err != nil {
		return nil, err
	}

	// ---- info ----
	info := []string{
		"版本：" + in.Version,
		"Go：" + runtime.Version(),
		"平台：" + runtime.GOOS + "/" + runtime.GOARCH,
		"运行时长：" + time.Since(diagStart).Round(time.Second).String(),
		"数据目录：" + dataDir(),
		"配置路径：" + in.ConfigPath,
		"",
	}
	if err := write("info.txt", []byte(strings.Join(info, "\n"))); err != nil {
		return nil, err
	}

	// ---- 配置（脱敏）----
	if err := writeJSON("config.redacted.json", redactedConfigMap(in.Config)); err != nil {
		return nil, err
	}

	// ---- 写入历史（去掉快照本体）----
	hist := make([]map[string]any, 0, len(in.History))
	for _, r := range in.History {
		hist = append(hist, map[string]any{
			"id": r.ID, "kind": r.kindOrDefault(), "name": r.Name,
			"person_id": r.PersonID, "item_id": r.ItemID,
			"created_at": r.CreatedAt, "sources": r.Sources,
			"changed": r.Changed, "rolled_back": r.RolledBack,
			"rollback_at": r.RollbackAt,
		})
	}
	if err := writeJSON("sync_history.json", hist); err != nil {
		return nil, err
	}

	// ---- 任务与日志 ----
	if err := writeJSON("jobs.json", in.Jobs); err != nil {
		return nil, err
	}

	// ---- 缓存清单 ----
	files := append([]diagCacheFile(nil), in.Cache...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	if err := writeJSON("cache.json", files); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- 接口 ----------

// handleDiagBundle 打包一份脱敏诊断包给用户下载。
//
// 只读：不改配置、不写缓存，唯一的副作用是读一遍目录。
func (a *App) handleDiagBundle(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()

	// 历史最多带 200 条：诊断看的是「最近出过什么岔子」，不是全量档案。
	hist := a.sync.ListKind("", 200)

	jobs := a.jobs.List(50)
	// 每个任务最多留最后 200 行日志 —— 一个批量任务的日志能有上千行，
	// 全塞进包里几十 KB 都是重复的进度行。
	for _, j := range jobs {
		if logs, ok := j["logs"].([]JobLog); ok && len(logs) > 200 {
			j["logs"] = logs[len(logs)-200:]
		}
	}

	blob, err := buildDiagZip(diagInput{
		Version:    appVersion,
		Config:     cfg,
		ConfigPath: a.store.Path(),
		History:    hist,
		Jobs:       jobs,
		Cache:      listCacheFiles(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("生成诊断包失败：%w", err))
		return
	}

	name := "emby-meta-editor-diag-" + time.Now().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("Content-Length", itoa(len(blob)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// listCacheFiles 列出数据目录下 cache/ 里的文件与大小。
//
// 只列一层：真正会出问题的就是那几个已知落盘文件（gfriends 索引、别名记忆、
// 同步历史），子目录里的东西对排障没帮助，反而可能带出不该外传的内容。
func listCacheFiles() []diagCacheFile {
	dir := cacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]diagCacheFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, diagCacheFile{Name: e.Name(), Size: info.Size()})
	}
	return out
}
