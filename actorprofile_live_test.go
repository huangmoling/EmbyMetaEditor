package main

// 对各资料源的**真实网络**冒烟。默认跳过，避免 CI / 日常单测依赖外网。
//
//	ACTOR_LIVE=1 go test -run TestLiveActorSources -v -count=1 .
//
// 只读：不碰 Emby、不写任何文件。用来回答「换了名字还能不能搜到」这类
// 夹具回答不了的问题（夹具只能验证解析，验证不了搜索流程）。

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveActorSources(t *testing.T) {
	if os.Getenv("ACTOR_LIVE") == "" {
		t.Skip("设置 ACTOR_LIVE=1 才跑真实网络冒烟")
	}
	cfg := Config{}
	client := newHTTPClient(cfg)

	cases := []struct {
		name    string
		aliases []string
	}{
		{"若宮穂乃", nil},
		{"坂井なな", nil},
		{"水户香奈", []string{"水戸かな", "Mito Kana"}},
		{"这个演员根本不存在xyz", nil},
	}

	for _, s := range actorSources() {
		t.Run(s.Label(), func(t *testing.T) {
			for _, c := range cases {
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				f, err := s.Fetch(ctx, client, c.name, c.aliases)
				cancel()
				switch {
				case err != nil:
					t.Logf("  %-22s 错误：%v", c.name, err)
				case f == nil:
					t.Logf("  %-22s 未命中", c.name)
				default:
					t.Logf("  %-22s 命中(%d) birth=%s place=%s h=%s B/W/H=%s/%s/%s cup=%s blood=%s agency=%q hobby=%q aliases=%d",
						c.name, f.MatchScore, f.BirthDate, f.BirthPlace, f.Height,
						f.Bust, f.Waist, f.Hip, f.Cup, f.BloodType, f.Agency, f.Hobby,
						len(f.Aliases))
				}
			}
		})
	}
}
