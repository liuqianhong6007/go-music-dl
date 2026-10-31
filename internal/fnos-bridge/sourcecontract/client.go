package sourcecontract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const TokenHeader = "X-MusicDL-Source-Token"
const EmbeddedPathPrefix = "/internal/source-api"

type Client struct {
	http   *http.Client
	token  string
	prefix string
}

func NewClient(socketPath, token string) *Client {
	return NewClientWithPrefix(socketPath, token, EmbeddedPathPrefix)
}

func NewClientWithPrefix(socketPath, token, prefix string) *Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
	}
	return &Client{
		http:   &http.Client{Transport: transport, Timeout: 2 * time.Minute},
		token:  token,
		prefix: strings.TrimRight(prefix, "/"),
	}
}

func (c *Client) Close() {
	if c == nil || c.http == nil {
		return
	}
	if transport, ok := c.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func (c *Client) Search(ctx context.Context, keyword string, sources []string, limit int) ([]Track, error) {
	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("sources", strings.Join(sources, ","))
	values.Set("limit", strconv.Itoa(limit))
	var tracks []Track
	if err := c.getJSON(ctx, "/v1/search?"+values.Encode(), &tracks); err != nil {
		return nil, err
	}
	return tracks, nil
}

func (c *Client) Health(ctx context.Context) error {
	var payload struct {
		OK bool `json:"ok"`
	}
	if err := c.getJSON(ctx, "/v1/health", &payload); err != nil {
		return err
	}
	if !payload.OK {
		return fmt.Errorf("source API is unhealthy")
	}
	return nil
}

func (c *Client) Stream(ctx context.Context, track Track, rangeHeader string, head bool) (*StreamResult, error) {
	method := http.MethodGet
	if head {
		method = http.MethodHead
	}
	request, err := c.request(ctx, method, "/v1/stream?"+trackQuery(track), nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("source stream returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return &StreamResult{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: response.Body}, nil
}

func (c *Client) EnqueueDownload(ctx context.Context, track Track) (DownloadResult, error) {
	body, err := json.Marshal(track)
	if err != nil {
		return DownloadResult{}, err
	}
	request, err := c.request(ctx, http.MethodPost, "/v1/download", bytes.NewReader(body))
	if err != nil {
		return DownloadResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return DownloadResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return DownloadResult{}, fmt.Errorf("download enqueue returned %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var result DownloadResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return DownloadResult{}, fmt.Errorf("decode download result: %w", err)
	}
	return result, nil
}

func (c *Client) DownloadStatus(ctx context.Context, id string) (DownloadState, bool, error) {
	response, err := c.do(ctx, http.MethodGet, "/v1/download/status?id="+url.QueryEscape(id), nil)
	if err != nil {
		return DownloadState{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return DownloadState{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return DownloadState{}, false, fmt.Errorf("download status returned %d", response.StatusCode)
	}
	var state DownloadState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		return DownloadState{}, false, err
	}
	return state, true, nil
}

func (c *Client) ForgetDownload(ctx context.Context, id string) error {
	response, err := c.do(ctx, http.MethodDelete, "/v1/download?id="+url.QueryEscape(id), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download delete returned %d", response.StatusCode)
	}
	return nil
}

func (c *Client) Lyric(ctx context.Context, track Track) (string, error) {
	var result struct {
		Lyric string `json:"lyric"`
	}
	if err := c.getJSON(ctx, "/v1/lyric?"+trackQuery(track), &result); err != nil {
		return "", err
	}
	return result.Lyric, nil
}

func (c *Client) Cover(ctx context.Context, track Track) ([]byte, string, error) {
	response, err := c.do(ctx, http.MethodGet, "/v1/cover?"+trackQuery(track), nil)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, "", nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cover returned %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	return body, response.Header.Get("Content-Type"), err
}

func (c *Client) getJSON(ctx context.Context, path string, target any) error {
	response, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("source API %s returned %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode source API response: %w", err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	request, err := c.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return c.http.Do(request)
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://source"+c.prefix+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		request.Header.Set(TokenHeader, c.token)
	}
	return request, nil
}

func trackQuery(track Track) string {
	values := url.Values{}
	values.Set("id", track.ID)
	values.Set("source", track.Source)
	values.Set("name", track.Name)
	values.Set("artist", track.Artist)
	values.Set("album", track.Album)
	values.Set("duration", strconv.Itoa(track.Duration))
	values.Set("size", strconv.FormatInt(track.Size, 10))
	values.Set("bitrate", strconv.Itoa(track.Bitrate))
	values.Set("ext", track.Ext)
	values.Set("cover", track.Cover)
	return values.Encode()
}
