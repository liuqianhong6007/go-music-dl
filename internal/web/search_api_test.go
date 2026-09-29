package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/music-lib/model"
)

func TestSearchAPIPagination(t *testing.T) {
	tests := []struct {
		name        string
		pageRaw     string
		pageSizeRaw string
		defaultSize int
		wantPage    int
		wantSize    int
	}{
		{name: "缺省参数用默认每页条数", wantPage: 1, wantSize: core.DefaultWebPageSize},
		{name: "默认值来自设置", defaultSize: 50, wantPage: 1, wantSize: 50},
		{name: "显式分页", pageRaw: "3", pageSizeRaw: "30", wantPage: 3, wantSize: 30},
		{name: "非法值回退默认", pageRaw: "abc", pageSizeRaw: "-1", wantPage: 1, wantSize: core.DefaultWebPageSize},
		{name: "零值回退默认", pageRaw: "0", pageSizeRaw: "0", wantPage: 1, wantSize: core.DefaultWebPageSize},
		{name: "每页上限 500", pageSizeRaw: "9999", wantPage: 1, wantSize: 500},
		{name: "空白裁剪", pageRaw: " 2 ", pageSizeRaw: " 10 ", wantPage: 2, wantSize: 10},
		{name: "设置为非正数回退常量", defaultSize: -5, wantPage: 1, wantSize: core.DefaultWebPageSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, size := searchAPIPagination(tt.pageRaw, tt.pageSizeRaw, tt.defaultSize)
			if page != tt.wantPage || size != tt.wantSize {
				t.Fatalf(
					"searchAPIPagination(%q, %q, %d) = (%d, %d), want (%d, %d)",
					tt.pageRaw, tt.pageSizeRaw, tt.defaultSize,
					page, size, tt.wantPage, tt.wantSize,
				)
			}
		})
	}
}

func TestPaginateSearchSongs(t *testing.T) {
	songs := make([]model.Song, 42)
	for i := range songs {
		songs[i] = model.Song{ID: strconv.Itoa(i + 1)}
	}

	t.Run("第一页", func(t *testing.T) {
		pageSongs, page, total, totalPages := paginateSearchSongs(songs, 1, 30)
		if page != 1 || total != 42 || totalPages != 2 || len(pageSongs) != 30 {
			t.Fatalf("page=%d total=%d totalPages=%d len=%d", page, total, totalPages, len(pageSongs))
		}
		if pageSongs[0].ID != "1" {
			t.Fatalf("首条 = %q, want 1", pageSongs[0].ID)
		}
	})

	t.Run("第二页只剩余数", func(t *testing.T) {
		pageSongs, page, _, _ := paginateSearchSongs(songs, 2, 30)
		if page != 2 || len(pageSongs) != 12 {
			t.Fatalf("page=%d len=%d, want page=2 len=12", page, len(pageSongs))
		}
		if pageSongs[0].ID != "31" {
			t.Fatalf("首条 = %q, want 31", pageSongs[0].ID)
		}
	})

	t.Run("超出末页收敛到末页而非空列表", func(t *testing.T) {
		pageSongs, page, _, _ := paginateSearchSongs(songs, 99, 30)
		if page != 2 || len(pageSongs) != 12 {
			t.Fatalf("page=%d len=%d, want page=2 len=12", page, len(pageSongs))
		}
	})

	t.Run("空结果算一页", func(t *testing.T) {
		pageSongs, page, total, totalPages := paginateSearchSongs(nil, 1, 30)
		if page != 1 || total != 0 || totalPages != 1 || len(pageSongs) != 0 {
			t.Fatalf("page=%d total=%d totalPages=%d len=%d", page, total, totalPages, len(pageSongs))
		}
	})

	t.Run("非法 page/pageSize 兜底", func(t *testing.T) {
		pageSongs, page, _, _ := paginateSearchSongs(songs, 0, 0)
		if page != 1 || len(pageSongs) != 1 {
			t.Fatalf("page=%d len=%d, want page=1 len=1", page, len(pageSongs))
		}
	})
}

func TestSearchAPIValidation(t *testing.T) {
	engine := newAppEngine(t, StartOptions{})

	t.Run("缺少 q 返回 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, RoutePrefix+"/api/search", nil)
		req.Header.Set("Accept", "application/json")
		if rec := doRequest(engine, req); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("不支持的 type 返回 400", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet,
			RoutePrefix+"/api/search?q=test&type=playlist",
			nil,
		)
		req.Header.Set("Accept", "application/json")
		if rec := doRequest(engine, req); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}

// 新增的 JSON 接口必须和网页端 /search 处在同一鉴权层级，否则会在
// --require-login 部署下变成一个匿名可用的搜索入口。
func TestSearchAPIRespectsRequireLogin(t *testing.T) {
	engine := newAppEngine(t, StartOptions{RequireLogin: true, GatewayAuth: true})

	t.Run("匿名被拒", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, RoutePrefix+"/api/search?q=test", nil)
		req.Header.Set("Accept", "application/json")
		if rec := doRequest(engine, req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("网关身份可通过鉴权到达处理器", func(t *testing.T) {
		// 刻意不带 q：鉴权通过后处理器会因缺参返回 400，以此证明请求确实
		// 走到了业务逻辑，而不是被中间件拦下。
		req := withGatewaySource(
			httptest.NewRequest(http.MethodGet, RoutePrefix+"/api/search", nil),
		)
		req.Header = gatewayHeaders("1002", "alice", "false")
		req.Header.Set("Accept", "application/json")
		if rec := doRequest(engine, req); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}
