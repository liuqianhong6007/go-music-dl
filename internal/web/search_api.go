package web

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/music-lib/model"
)

// JSON 搜索接口，供第三方客户端（例如飞牛音乐安卓客户端）使用。
//
// 背景：项目原有的 `GET /search` 返回服务端渲染的 HTML，客户端只能解析
// `li.song-card` 上的 data-* 属性——上游一改模板，客户端就会**静默**失效。
// 这里复用同一套 `core.GetSearchFunc`，只是把结果按稳定的 JSON 结构输出。
//
// 路由注册在 `RegisterMusicRoutes` 的 api 组上，与 `/search` 同组，因此
// `--require-login` / 网关鉴权的行为与网页端完全一致，不会成为新的匿名入口。
func registerSearchAPIRoutes(api *gin.RouterGroup) {
	api.GET("/api/search", handleSearchAPI)
}

// searchAPIResult 是 `/api/search` 的响应。
//
// 字段命名沿用项目其它 JSON 接口的扁平风格（如 `/inspect`、`/download`），
// 不套 `{code,msg,data}` 信封——那是飞牛音乐 API 的约定，不是本项目的。
type searchAPIResult struct {
	Keyword    string       `json:"keyword"`
	SearchType string       `json:"search_type"`
	Sources    []string     `json:"sources"`
	Page       int          `json:"page"`
	PageSize   int          `json:"page_size"`
	Total      int          `json:"total"`
	TotalPages int          `json:"total_pages"`
	Songs      []model.Song `json:"songs"`
}

func handleSearchAPI(c *gin.Context) {
	keyword := strings.TrimSpace(c.Query("q"))
	if keyword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少参数 q"})
		return
	}

	searchType := strings.ToLower(strings.TrimSpace(c.DefaultQuery("type", "song")))
	if searchType != "song" {
		// 目前只做单曲；歌单/专辑仍走网页端或后续按需扩展。
		c.JSON(http.StatusBadRequest, gin.H{"error": "该接口当前只支持 type=song"})
		return
	}

	sources := c.QueryArray("sources")
	if len(sources) == 0 {
		sources = defaultSourcesForSearchType(searchType)
	}

	page, pageSize := searchAPIPagination(
		c.Query("page"),
		c.Query("page_size"),
		core.GetWebSettings().WebPageSize,
	)
	pageSongs, page, total, totalPages := paginateSearchSongs(
		searchSongsForAPI(keyword, sources, strings.TrimSpace(c.Query("exact_artist"))),
		page,
		pageSize,
	)

	c.JSON(http.StatusOK, searchAPIResult{
		Keyword:    keyword,
		SearchType: searchType,
		Sources:    sources,
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		Songs:      pageSongs,
	})
}

// paginateSearchSongs 按页切片，并把 page 收敛到有效范围——语义与 renderIndex
// 一致（超出末页时回到末页，而不是返回空列表）。
func paginateSearchSongs(songs []model.Song, page, pageSize int) (
	pageSongs []model.Song, pageOut int, total int, totalPages int,
) {
	if pageSize < 1 {
		pageSize = 1
	}
	if page < 1 {
		page = 1
	}

	total = len(songs)
	totalPages = 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
		if page > totalPages {
			page = totalPages
		}
	}

	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return songs[start:end], page, total, totalPages
}

// searchAPIPagination 与 renderIndex 使用同一套分页语义，避免网页端与接口端
// 对同一个 `page_size` 给出不同结果。
//
// 抽成纯函数（默认值由调用方传入）以便单测，不需要初始化设置库。
func searchAPIPagination(pageRaw, pageSizeRaw string, defaultPageSize int) (page int, pageSize int) {
	pageSize = defaultPageSize
	if pageSize <= 0 {
		pageSize = core.DefaultWebPageSize
	}
	if raw := strings.TrimSpace(pageSizeRaw); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			pageSize = n
		}
	}
	if pageSize > 500 {
		pageSize = 500
	}

	page = 1
	if raw := strings.TrimSpace(pageRaw); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}
	return page, pageSize
}

// searchSongsForAPI 并发遍历各音源并合并结果。
//
// 与 `/search` 网页端保持一致：跳过 music-dl 自己的本地音乐源、按需并入本地
// 曲库命中、最后按 exact_artist 过滤。返回顺序即音源顺序，与网页端一致。
func searchSongsForAPI(keyword string, sources []string, exactArtist string) []model.Song {
	var allSongs []model.Song
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, src := range sources {
		if isLocalMusicSource(src) {
			continue
		}
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			fn := core.GetSearchFunc(s)
			if fn == nil {
				return
			}
			res, err := fn(keyword)
			if err != nil {
				return
			}
			for i := range res {
				res[i].Source = s
			}
			mu.Lock()
			allSongs = append(allSongs, res...)
			mu.Unlock()
		}(src)
	}
	wg.Wait()

	if containsLocalSource(sources) {
		if local := localMusicSearchSongs(keyword, 200); len(local) > 0 {
			allSongs = append(allSongs, local...)
		}
	}

	if exactArtist != "" && len(allSongs) > 0 {
		allSongs = filterSongsByExactArtist(allSongs, exactArtist)
	}
	return allSongs
}
