package main

// 演员资料管理的 HTTP 接口。放在单独文件里，避免 api.go 继续膨胀。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// profileRequest 是抓取/写入共用的入参。
type profileRequest struct {
	PersonID     string   `json:"person_id"`
	Name         string   `json:"name"`
	Sources      []string `json:"sources"`
	Keys         []string `json:"keys"`           // 只写这些字段；空 = 全部可写的
	UseAliasMemo *bool    `json:"use_alias_memo"` // 默认 true
}

func (in profileRequest) opts() FetchOptions {
	useMemo := true
	if in.UseAliasMemo != nil {
		useMemo = *in.UseAliasMemo
	}
	return FetchOptions{Sources: in.Sources, UseAliasMemo: useMemo}
}

// handleProfileSources 列出可用的资料源（供界面渲染勾选项）。
func (a *App) handleProfileSources(w http.ResponseWriter, r *http.Request) {
	type srcView struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	}
	var out []srcView
	for _, s := range actorSources() {
		out = append(out, srcView{Key: s.Key(), Label: s.Label()})
	}
	writeOK(w, map[string]any{
		"sources":        out,
		"alias_groups":   a.aliases.Count(),
		"history_count":  len(a.sync.List(0)),
		"write_strategy": "only_blank",
	})
}

// handleProfilePreview 抓取资料并返回「现有值 vs 抓取值」对照，**不写任何东西**。
func (a *App) handleProfilePreview(w http.ResponseWriter, r *http.Request) {
	var in profileRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少演员名"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	prof, err := a.fetchActorProfile(ctx, in.PersonID, in.Name, in.opts())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, prof)
}

// handleProfileApply 抓取并写入（只填空白字段）。
func (a *App) handleProfileApply(w http.ResponseWriter, r *http.Request) {
	var in profileRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.PersonID) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少演员 ID"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	res, err := a.applyActorProfile(ctx, in.PersonID, in.Name, in.Keys, in.opts())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}

// profileBatchItem 是界面传进来的一个处理目标。带 ID 最稳（界面「批量处理当前页」走这条）。
type profileBatchItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// profileTarget 是最终确定要处理的一条：**ID 一定非空**。
type profileTarget struct{ ID, Name string }

// profileTargets 把批量入参整理成处理清单。
//
// 三条路径，优先级从高到低：
//  1. items —— 界面已经知道 ID，直接用（最精确，不受服务端列表分页/排序影响）；
//  2. names —— 只有名字，逐个 `PersonByName` 解析出 ID；
//  3. 都没有 —— 按 limit + parentID 拉列表。
//
// **不管走哪条，返回的目标 ID 都必须非空。** 这是这个函数存在的唯一理由：
// applyActorProfile 拿到空 ID 会直接报「缺少演员 ID」，但它前面那次 fetchActorProfile
// 因为读不到 Emby 现有值，会把每个字段都判成「空白、将写入」—— 日志上看着像要成功，
// 实际全失败，而且批量任务里这种失败特别难和「真的没资料」区分开。
func profileTargets(ctx context.Context, e *Emby, items []profileBatchItem, names []string, limit int, parentID string) ([]profileTarget, error) {
	var raw []profileTarget
	for _, it := range items {
		if n := strings.TrimSpace(it.Name); n != "" {
			raw = append(raw, profileTarget{ID: strings.TrimSpace(it.ID), Name: n})
		}
	}
	if len(raw) == 0 {
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				raw = append(raw, profileTarget{Name: n})
			}
		}
	}
	if len(raw) == 0 {
		if limit <= 0 {
			limit = 100
		}
		start := 0
		for len(raw) < limit {
			pr, err := e.Persons(ctx, start, 500, "", parentID)
			if err != nil {
				return nil, err
			}
			for _, p := range pr.Items {
				raw = append(raw, profileTarget{ID: p.Id, Name: p.Name})
				if len(raw) >= limit {
					break
				}
			}
			start += 500
			if start >= pr.TotalRecordCount || len(pr.Items) == 0 {
				break
			}
		}
	}

	out := raw[:0]
	for _, t := range raw {
		if t.ID != "" {
			out = append(out, t)
			continue
		}
		p, err := e.PersonByName(ctx, t.Name)
		if err != nil || p == nil || p.Id == "" {
			continue // 解析不到就丢弃，绝不把空 ID 传下去
		}
		out = append(out, profileTarget{ID: p.Id, Name: p.Name})
	}
	return out, nil
}

// handleProfileBatch 批量抓取并写入，走任务系统（可在界面上看进度）。
func (a *App) handleProfileBatch(w http.ResponseWriter, r *http.Request) {
	var batch struct {
		Items        []profileBatchItem `json:"items"`
		Names        []string           `json:"names"`
		Limit        int                `json:"limit"`
		ParentID     string             `json:"parent_id"`
		Sources      []string           `json:"sources"`
		Keys         []string           `json:"keys"`
		UseAliasMemo *bool              `json:"use_alias_memo"`
	}
	if err := decodeBody(r, &batch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	cfg := a.store.Get()
	e := NewEmby(cfg)
	ctx := r.Context()

	targets, err := profileTargets(ctx, e, batch.Items, batch.Names, batch.Limit, batch.ParentID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("没有需要处理的演员"))
		return
	}

	useMemo := true
	if batch.UseAliasMemo != nil {
		useMemo = *batch.UseAliasMemo
	}
	opts := FetchOptions{Sources: batch.Sources, UseAliasMemo: useMemo}
	keys := batch.Keys
	job, jobCtx := a.jobs.New("actor-profile",
		fmt.Sprintf("批量抓取演员资料（%d 位）", len(targets)), len(targets))
	job.addLog("info", fmt.Sprintf("开始处理 %d 位演员，写入策略：只填空白字段", len(targets)))

	go func() {
		sem := make(chan struct{}, maxInt(1, cfg.Concurrency))
		var wg sync.WaitGroup
		for _, t := range targets {
			if jobCtx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(t profileTarget) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				octx, cancel := context.WithTimeout(jobCtx, 90*time.Second)
				defer cancel()
				res, err := a.applyActorProfile(octx, t.ID, t.Name, keys, opts)
				job.mu.Lock()
				if err != nil {
					job.Failed++
					job.mu.Unlock()
					job.addLog("error", fmt.Sprintf("%s：%v", t.Name, err))
					return
				}
				if len(res.Written) == 0 {
					job.Skipped++
				} else {
					job.Done++
				}
				job.mu.Unlock()
				job.addLog("ok", fmt.Sprintf("%s：%s", t.Name, res.Message))
			}(t)
		}
		wg.Wait()
		if jobCtx.Err() != nil {
			job.setStatus("canceled")
		} else {
			job.setStatus("done")
		}
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

// handleProfileHistory 同步历史列表（不含快照本体）。
func (a *App) handleProfileHistory(w http.ResponseWriter, r *http.Request) {
	limit := atoiSafe(r.URL.Query().Get("limit"))
	writeOK(w, map[string]any{"records": a.sync.List(limit)})
}

// handleProfileRollback 一键回滚某次写入。
func (a *App) handleProfileRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := a.rollbackSync(ctx, in.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}

// handleProfileAliases 别名记忆概览。
func (a *App) handleProfileAliases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	a.aliases.mu.RLock()
	defer a.aliases.mu.RUnlock()
	out := []AliasGroup{}
	for _, g := range a.aliases.Groups {
		if q == "" || strings.Contains(g.CanonicalName, q) {
			out = append(out, g)
			if len(out) >= 200 {
				break
			}
		}
	}
	writeOK(w, map[string]any{"total": len(a.aliases.Groups), "groups": out})
}
