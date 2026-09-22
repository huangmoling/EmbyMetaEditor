package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// meServer 造一个可以精确控制 /Users/Me、/Users/{id}、/Users 三个接口行为的服务端，
// 用来验证 Me() 的多级回退。
//
// 背景：Emby 4.9.x 上用 API Key 调 /Users/Me 会 500（Unrecognized Guid format），
// 但同一个 Key 调 /Users/{id}、/Items 完全正常。
type meServer struct {
	srv        *httptest.Server
	meStatus   int
	meBody     string
	userStatus int
	userBody   string
	listStatus int
	listBody   string
	hits       []string
}

func newMeServer(t *testing.T) *meServer {
	t.Helper()
	s := &meServer{
		meStatus:   200,
		meBody:     `{"Id":"u1","Name":"admin"}`,
		userStatus: 200,
		userBody:   `{"Id":"u1","Name":"admin"}`,
		listStatus: 200,
		listBody:   `[{"Id":"u1","Name":"admin"}]`,
	}
	mux := http.NewServeMux()
	record := func(name string) { s.hits = append(s.hits, name) }
	mux.HandleFunc("GET /Users/Me", func(w http.ResponseWriter, r *http.Request) {
		record("Me")
		if s.meStatus != 200 {
			http.Error(w, s.meBody, s.meStatus)
			return
		}
		_, _ = w.Write([]byte(s.meBody))
	})
	mux.HandleFunc("GET /Users", func(w http.ResponseWriter, r *http.Request) {
		record("List")
		if s.listStatus != 200 {
			http.Error(w, s.listBody, s.listStatus)
			return
		}
		_, _ = w.Write([]byte(s.listBody))
	})
	mux.HandleFunc("GET /Users/{uid}", func(w http.ResponseWriter, r *http.Request) {
		record("ByID:" + r.PathValue("uid"))
		if s.userStatus != 200 {
			http.Error(w, s.userBody, s.userStatus)
			return
		}
		_, _ = w.Write([]byte(s.userBody))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func TestMeDirectOK(t *testing.T) {
	m := newMeServer(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "k"})
	lr, err := e.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lr.UserID != "u1" || lr.UserName != "admin" {
		t.Errorf("结果错误：%+v", lr)
	}
	if len(m.hits) != 1 || m.hits[0] != "Me" {
		t.Errorf("直接成功时不该走回退：%v", m.hits)
	}
}

// 这是本机实测遇到的真实形态：/Users/Me 500，但已知 userId 能查到。
func TestMeFallsBackToConfiguredUserID(t *testing.T) {
	m := newMeServer(t)
	m.meStatus = 500
	m.meBody = "Unrecognized Guid format."

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "api-key", UserID: "u1"})
	lr, err := e.Me(context.Background())
	if err != nil {
		t.Fatalf("应该回退成功，却报错：%v", err)
	}
	if lr.UserID != "u1" || lr.UserName != "admin" {
		t.Errorf("结果错误：%+v", lr)
	}
	if len(m.hits) < 2 || m.hits[0] != "Me" || m.hits[1] != "ByID:u1" {
		t.Errorf("回退顺序不对：%v", m.hits)
	}
}

// 没有配置 userId 时，退到 /Users 列表。
func TestMeFallsBackToUserList(t *testing.T) {
	m := newMeServer(t)
	m.meStatus = 500
	m.listBody = `[{"Id":"u9","Name":"someone"}]`

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "api-key"})
	lr, err := e.Me(context.Background())
	if err != nil {
		t.Fatalf("应该回退成功，却报错：%v", err)
	}
	if lr.UserID != "u9" || lr.UserName != "someone" {
		t.Errorf("结果错误：%+v", lr)
	}
}

// 列表里多个用户时，应该按已知用户名匹配，而不是盲取第一个。
func TestMeUserListPrefersNameMatch(t *testing.T) {
	m := newMeServer(t)
	m.meStatus = 500
	m.listBody = `[{"Id":"u1","Name":"alice"},{"Id":"u2","Name":"bob"},{"Id":"u3","Name":"carol"}]`

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "api-key", UserName: "bob"})
	lr, err := e.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lr.UserID != "u2" || lr.UserName != "bob" {
		t.Errorf("应选中 bob(u2)，实际 %+v", lr)
	}
}

// 配置里的 userId 是脏数据时，继续往下退到列表。
func TestMeFallsBackWhenConfiguredIDStale(t *testing.T) {
	m := newMeServer(t)
	m.meStatus = 500
	m.userStatus = 404
	m.userBody = "not found"
	m.listBody = `[{"Id":"u7","Name":"real"}]`

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "k", UserID: "stale-id"})
	lr, err := e.Me(context.Background())
	if err != nil {
		t.Fatalf("应该继续回退到列表：%v", err)
	}
	if lr.UserID != "u7" {
		t.Errorf("结果错误：%+v", lr)
	}
}

// 三条路都断了才报错，并且要把 /Users/Me 的原始错误带出来。
func TestMeAllPathsFail(t *testing.T) {
	m := newMeServer(t)
	m.meStatus = 500
	m.meBody = "Unrecognized Guid format."
	m.listStatus = 500
	m.listBody = "boom"

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "k"})
	_, err := e.Me(context.Background())
	if err == nil {
		t.Fatal("全部失败时应该报错")
	}
	if !strings.Contains(err.Error(), "Unrecognized Guid format") {
		t.Errorf("应保留 /Users/Me 的原始错误：%v", err)
	}
}

// /Users/Me 返回 200 但空对象（部分代理会这样）时，也要走回退。
func TestMeEmptyObjectFallsBack(t *testing.T) {
	m := newMeServer(t)
	m.meBody = `{}`
	m.listBody = `[{"Id":"u5","Name":"x"}]`

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "k"})
	lr, err := e.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lr.UserID != "u5" {
		t.Errorf("空对象应触发回退：%+v", lr)
	}
}
