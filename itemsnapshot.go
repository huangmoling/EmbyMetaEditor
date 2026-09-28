package main

// 条目（影片）写入的快照与回滚。
//
// 为什么需要：Emby 的 `POST /Items/{id}` 是**整对象替换**，服务端没有版本历史 ——
// 旧值在写下去的那一刻就永久没了。而这个工具的写路径特别多（批量刮削、国产传媒、
// 手动编辑），一次误操作就能改掉几十条元数据，且无处可退。
// 演员资料那条路径早就有 `SyncStore` 快照 + 回滚了（`profile.go`），
// 但条目路径一条都没接 —— 这个文件就是把那套能力接到条目上。
//
// 复用同一个 `cache/sync_history.json`，用 `Kind` 区分两类记录，
// 这样界面上一个「写入历史」就能同时看到人物和条目。
//
// ⚠️ **图片不可还原**：快照只覆盖 DTO 里的元数据字段。
// 一次刮削覆盖掉的旧海报，回滚之后不会回来（Emby 也不留旧图）——
// 所以提示文案里必须写清楚，别让用户以为「回滚」等于「什么都没发生过」。

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// itemListFields 是「值是数组」的条目字段。
//
// 回滚到「原来没有值」时，必须按类型给出空值：清空列表要发 `[]`，
// 发 `null` 会被 `updateItem` 的 nil 防护（`v == nil || isNilVal(v)`）直接跳过，
// 于是 Emby 里那个字段**根本没被还原**，而界面显示「已回滚」。
// （人物回滚那边踩的就是这个坑，见 profile.go:rollbackSync 的注释。）
var itemListFields = map[string]bool{
	"Tags": true, "TagItems": true, "Genres": true, "Studios": true,
	"People": true, "ProductionLocations": true, "Countries": true,
	"Directors": true, "Writers": true, "Producers": true,
}

var itemMapFields = map[string]bool{
	"ProviderIds": true, "UserData": true,
}

var itemNumFields = map[string]bool{
	"ProductionYear": true, "CommunityRating": true, "RunTimeTicks": true,
	"IndexNumber": true, "ParentIndexNumber": true, "CriticRating": true,
}

// emptyForItemField 返回某个字段「清空」时该写什么。
func emptyForItemField(name string) any {
	switch {
	case itemListFields[name]:
		return []any{}
	case itemMapFields[name]:
		return map[string]any{}
	case itemNumFields[name]:
		return 0
	default:
		return ""
	}
}

// recordItemWrite 在写条目之前留一份快照，返回记录 ID（空串表示没记）。
//
// 只记**这次真的要写**的字段：没进 patch 的字段不该出现在回滚范围里，
// 否则回滚会把用户后来手动改的东西一起抹掉。
func (a *App) recordItemWrite(item Item, patch map[string]any, source string) string {
	if a.sync == nil || len(patch) == 0 {
		return ""
	}
	before := map[string]any{}
	after := map[string]any{}
	changed := make([]string, 0, len(patch))
	for k, v := range patch {
		// 与 updateItem 的防护保持一致：类型化的 nil 也会被它跳过，
		// 那就不是「这次要写」的字段，不该记进快照。
		if v == nil || isNilVal(v) {
			continue
		}
		before[k] = item[k] // 原来没这个字段时是 nil，回滚时按类型补空值
		after[k] = v
		changed = append(changed, k)
	}
	if len(changed) == 0 {
		return ""
	}
	sort.Strings(changed)
	id, _ := item["Id"].(string)
	name, _ := item["Name"].(string)
	return a.sync.Add(SyncRecord{
		Kind:    syncKindItem,
		ItemID:  id,
		Name:    name,
		Sources: []string{source},
		Changed: changed,
		Before:  before,
		After:   after,
	})
}

// recordItemWriteByID 是 recordItemWrite 的「只有 ID」版本，
// 供已经拿到 itemID、手上却没有完整 DTO 的调用方使用。
func (a *App) recordItemWriteByID(ctx context.Context, e *Emby, itemID string, patch map[string]any, source string) string {
	if a.sync == nil || len(patch) == 0 {
		return ""
	}
	item, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		// 读不到当前值就退化成「以空为底」的快照：至少 After 有记录，
		// 回滚时会把字段清空 —— 比「根本没有历史」好，但比读到当前值差。
		item = Item{"Id": itemID}
	}
	return a.recordItemWrite(item, patch, source)
}

// rollbackItemSync 把某个条目的某次写入还原回写入前的样子。
//
// 与人物资料回滚同源，但字段名直接用 Emby 的（Name / Overview / Tags…）：
// 条目的字段集合是整个 DTO，硬编码一张「人类可读标签表」维护不过来。
func (a *App) rollbackItemSync(ctx context.Context, recordID string) (*ApplyResult, error) {
	rec, ok := a.sync.Get(recordID)
	if !ok {
		return nil, fmt.Errorf("找不到这条写入记录")
	}
	if rec.kindOrDefault() != syncKindItem {
		return nil, fmt.Errorf("这条记录是人物资料写入，请到「演员资料」里回滚")
	}
	if rec.RolledBack {
		return nil, fmt.Errorf("这条记录已经回滚过了")
	}
	if rec.ItemID == "" {
		return nil, fmt.Errorf("记录里没有条目 ID，无法回滚")
	}

	patch := map[string]any{}
	for _, k := range rec.Changed {
		v, has := rec.Before[k]
		if !has || v == nil || isNilVal(v) {
			// 原来就没有这个值 → 明确写回该类型的空值。
			// 不补这一步的话，新值会留在 Emby 里，回滚等于没回。
			patch[k] = emptyForItemField(k)
			continue
		}
		patch[k] = v
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("这条记录没有可还原的字段")
	}

	// 用 Exact：UpdateItem 对 ProviderIds 是**只增不减**的合并语义，
	// 回滚不掉刮削新加进去的外部 ID。
	e := NewEmby(a.store.Get())
	if err := e.UpdateItemExact(ctx, rec.ItemID, patch); err != nil {
		return nil, fmt.Errorf("回滚失败：%w", err)
	}
	a.sync.MarkRolledBack(recordID)
	return &ApplyResult{
		ItemID:   rec.ItemID,
		Name:     rec.Name,
		Written:  sortedKeys(patch),
		RecordID: recordID,
		Message:  "已还原到写入前的元数据（图片不在快照范围内，覆盖掉的旧图不会回来）",
	}, nil
}

// ---------- 接口 ----------

// handleItemsHistory 列出最近的条目写入（只读）。
//
// 返回列表里带 `record_id`，前端拿它调 /api/items/rollback 撤销。
func (a *App) handleItemsHistory(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if s := strings.TrimSpace(r.URL.Query().Get("limit")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	if limit == 0 || limit > 200 {
		limit = 50
	}
	recs := a.sync.ListKind(syncKindItem, limit)
	writeOK(w, map[string]any{
		"records": recs,
		"total":   len(recs),
		// 图片不参与快照，界面必须把这句话显示出来 ——
		// 否则用户会以为「回滚」能把覆盖掉的旧海报也一起找回来。
		"note": "快照只覆盖元数据字段；被覆盖的旧图片无法还原。",
	})
}

// handleItemsRollback 撤销某次条目写入。
func (a *App) handleItemsRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少记录 id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := a.rollbackItemSync(ctx, in.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}
