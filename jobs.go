package main

import (
	"context"
	"sync"
	"time"
)

// JobLog 是一条任务日志。
type JobLog struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Job 表示一个后台批处理任务，前端通过轮询获取进度。
type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Total    int       `json:"total"`
	Done     int       `json:"done"`
	Failed   int       `json:"failed"`
	Skipped  int       `json:"skipped"`
	Status   string    `json:"status"` // running / done / failed / canceled
	Err      string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Logs     []JobLog  `json:"logs"`
	Result   any       `json:"result,omitempty"`

	mu     sync.Mutex
	cancel context.CancelFunc
	ctx    context.Context
}

const maxJobLogs = 1500

func (j *Job) addLog(level, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Logs = append(j.Logs, JobLog{Time: time.Now(), Level: level, Message: msg})
	if len(j.Logs) > maxJobLogs {
		j.Logs = j.Logs[len(j.Logs)-maxJobLogs:]
	}
}

func (j *Job) setStatus(s string) {
	j.mu.Lock()
	j.Status = s
	if s != "running" {
		j.Finished = time.Now()
	}
	j.mu.Unlock()
}

// Snapshot 返回可序列化的进度快照。
func (j *Job) Snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	status := j.Status
	total, done, failed, skipped := j.Total, j.Done, j.Failed, j.Skipped
	percent := 0
	if total > 0 {
		percent = (done + failed + skipped) * 100 / total
		if percent > 100 {
			percent = 100
		}
	}
	out := map[string]any{
		"id":      j.ID,
		"kind":    j.Kind,
		"title":   j.Title,
		"total":   total,
		"done":    done,
		"failed":  failed,
		"skipped": skipped,
		"status":  status,
		"percent": percent,
		"started": j.Started.Format(time.RFC3339),
		"logs":    j.Logs,
		"error":   j.Err,
		"result":  j.Result,
	}
	if !j.Finished.IsZero() {
		out["finished"] = j.Finished.Format(time.RFC3339)
	}
	return out
}

// JobRegistry 管理所有后台任务。
type JobRegistry struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// NewJobRegistry 创建任务注册表。
func NewJobRegistry() *JobRegistry {
	r := &JobRegistry{jobs: map[string]*Job{}}
	go r.gcLoop()
	return r
}

// New 创建并登记一个任务，返回任务与其可取消的 context。
func (r *JobRegistry) New(kind, title string, total int) (*Job, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		ID:      randHex(8),
		Kind:    kind,
		Title:   title,
		Total:   total,
		Status:  "running",
		Started: time.Now(),
		cancel:  cancel,
		ctx:     ctx,
	}
	r.mu.Lock()
	r.jobs[j.ID] = j
	r.mu.Unlock()
	return j, ctx
}

// Get 按 ID 取任务。
func (r *JobRegistry) Get(id string) *Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.jobs[id]
}

// Cancel 取消任务。
func (r *JobRegistry) Cancel(id string) bool {
	j := r.Get(id)
	if j == nil {
		return false
	}
	j.mu.Lock()
	c := j.cancel
	j.mu.Unlock()
	if c != nil {
		c()
		return true
	}
	return false
}

// List 返回最近的任务快照。
func (r *JobRegistry) List(limit int) []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j.Snapshot())
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// gcLoop 定期清理 1 小时前结束的任务。
func (r *JobRegistry) gcLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-1 * time.Hour)
		r.mu.Lock()
		for id, j := range r.jobs {
			j.mu.Lock()
			fin := j.Finished
			j.mu.Unlock()
			if !fin.IsZero() && fin.Before(cutoff) {
				delete(r.jobs, id)
			}
		}
		r.mu.Unlock()
	}
}
