// Package api 暴露 HTTP API 与 SSE 事件流。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gosync/internal/config"
	"gosync/internal/device"
	"gosync/internal/discovery"
	"gosync/internal/folders"
	syncpkg "gosync/internal/sync"
)

// Server 持有 HTTP 服务所需的全部依赖。
type Server struct {
	Cfg       *config.Store
	DeviceID  string
	SyncHub   *syncpkg.Hub
	Discovery *discovery.Service
	LogDir    string

	mu        sync.Mutex
	logFiles  map[string]string // jobID -> 日志文件路径
	jobCancel map[string]context.CancelFunc
}

// NewServer 构造 API Server。
func NewServer(cfg *config.Store, hub *syncpkg.Hub, disc *discovery.Service, logDir string) (*Server, error) {
	return NewServerWithID(cfg, hub, disc, logDir, "")
}

// NewServerWithID 构造 API Server，deviceID 非空时覆盖自动生成的 ID（用于多实例/测试）。
func NewServerWithID(cfg *config.Store, hub *syncpkg.Hub, disc *discovery.Service, logDir, deviceID string) (*Server, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	if deviceID == "" {
		var err error
		deviceID, err = device.ID()
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		Cfg:       cfg,
		DeviceID:  deviceID,
		SyncHub:   hub,
		Discovery: disc,
		LogDir:    logDir,
		logFiles:  make(map[string]string),
		jobCancel: make(map[string]context.CancelFunc),
	}, nil
}

// Routes 注册全部路由到给定 mux。
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/folders", s.handleFolders)
	mux.HandleFunc("/api/folders/list", s.handleFolderList)
	mux.HandleFunc("/api/peers", s.handlePeers)
	mux.HandleFunc("/api/peers/discovered", s.handleDiscovered)
	mux.HandleFunc("/api/peers/connect", s.handleConnect)
	mux.HandleFunc("/api/sync/start", s.handleSyncStart)
	mux.HandleFunc("/api/sync/cancel", s.handleSyncCancel)
	mux.HandleFunc("/api/sync/log", s.handleSyncLog)
	mux.HandleFunc("/api/sync/log/download", s.handleSyncLogDownload)
	mux.HandleFunc("/api/sync/jobs", s.handleSyncJobs)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/peer/manifest", s.handlePeerManifest)
	mux.HandleFunc("/api/peer/file", s.handlePeerFile)
	mux.HandleFunc("/api/peer/info", s.handlePeerInfo)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	snap := s.Cfg.Snapshot()
	writeJSON(w, map[string]any{
		"device_id":    s.DeviceID,
		"device_name":  snap.DeviceName,
		"listen":       snap.Listen,
		"discovery_on": snap.DiscoveryOn,
		"folder_count": len(snap.Folders),
		"peer_count":   len(snap.Peers),
	})
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		paths := s.Cfg.Snapshot().Folders
		type item struct {
			Path     string `json:"path"`
			NumFiles int    `json:"num_files"`
			TotalSz  int64  `json:"total_size"`
			Exists   bool   `json:"exists"`
			Error    string `json:"error,omitempty"`
		}
		out := make([]item, 0, len(paths))
		for _, p := range paths {
			it := item{Path: p}
			summary, err := folders.ScanSummary(p)
			if err != nil {
				it.Error = err.Error()
				out = append(out, it)
				continue
			}
			it.NumFiles = summary.NumFiles
			it.TotalSz = summary.TotalSz
			it.Exists = true
			out = append(out, it)
		}
		writeJSON(w, out)
	case http.MethodPost:
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Path) == "" {
			http.Error(w, "path empty", http.StatusBadRequest)
			return
		}
		if err := s.Cfg.AddFolder(body.Path); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case http.MethodDelete:
		p := r.URL.Query().Get("path")
		if err := s.Cfg.RemoveFolder(p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleFolderList(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path empty", http.StatusBadRequest)
		return
	}
	registered := s.isRegisteredFolder(p)
	if !registered {
		http.Error(w, "folder not registered", http.StatusForbidden)
		return
	}
	includeHash := r.URL.Query().Get("hash") == "1"
	var (
		files []folders.FileInfo
		err   error
	)
	if includeHash {
		files, err = folders.ListFiles(p, nil)
	} else {
		files, err = folders.ListFilesFast(p)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.Cfg.Snapshot().Peers)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if err := s.Cfg.RemovePeer(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConnect 用户输入对端 ID 后调用：
// 1. 优先从已发现的 beacon 中按 ID 查找（包含 address）；
// 2. 若本地没有该 ID 的发现记录，则要求前端同时传 address。
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Address string `json:"address"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		http.Error(w, "id empty", http.StatusBadRequest)
		return
	}
	if body.ID == s.DeviceID {
		http.Error(w, "cannot connect to self", http.StatusBadRequest)
		return
	}
	// 先到 beacon 缓存里查
	if body.Address == "" && s.Discovery != nil {
		for _, b := range s.Discovery.Peers() {
			if b.ID == body.ID {
				body.Address = b.Address
				if body.Name == "" {
					body.Name = b.Name
				}
				break
			}
		}
	}
	// 探测对端是否可达
	if body.Address == "" {
		http.Error(w, "对端未发现，请填写 address 或先打开对端软件", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(body.Address, "http://") && !strings.HasPrefix(body.Address, "https://") {
		body.Address = "http://" + body.Address
	}
	// 健康检查
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(body.Address, "/")+"/api/peer/info", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "无法连接对端: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, fmt.Sprintf("对端返回 %s", resp.Status), http.StatusBadGateway)
		return
	}
	var info struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&info)
	if info.ID != "" && info.ID != body.ID {
		http.Error(w, fmt.Sprintf("对端 ID 不匹配：声称 %s 实际 %s", body.ID, info.ID), http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		body.Name = info.Name
	}
	if err := s.Cfg.UpsertPeer(config.Peer{ID: body.ID, Name: body.Name, Address: body.Address}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"ok":      true,
		"id":      body.ID,
		"name":    body.Name,
		"address": body.Address,
	})
}

func (s *Server) handleDiscovered(w http.ResponseWriter, r *http.Request) {
	if s.Discovery == nil {
		writeJSON(w, []discovery.Beacon{})
		return
	}
	writeJSON(w, s.Discovery.Peers())
}

func (s *Server) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		PeerID  string `json:"peer_id"`
		Folder  string `json:"folder"`        // 本端文件夹
		Remote  string `json:"remote_folder"` // 可选：对端文件夹；缺省与 Folder 相同
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.isRegisteredFolder(body.Folder) {
		http.Error(w, "folder not registered locally", http.StatusBadRequest)
		return
	}
	if body.Remote == "" {
		body.Remote = body.Folder
	}
	snap := s.Cfg.Snapshot()
	var peer *config.Peer
	for i, p := range snap.Peers {
		if p.ID == body.PeerID {
			peer = &snap.Peers[i]
			break
		}
	}
	if peer == nil {
		http.Error(w, "peer not connected", http.StatusBadRequest)
		return
	}
	jobID := newJobID()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.jobCancel[jobID] = cancel
	s.mu.Unlock()
	go func() {
		result, err := syncpkg.RunPush(ctx, s.SyncHub, jobID, body.Folder, peer.Address, body.Remote)
		logPath := filepath.Join(s.LogDir, jobID+".log")
		_ = writeJobLog(logPath, result, err)
		s.mu.Lock()
		delete(s.jobCancel, jobID)
		s.logFiles[jobID] = logPath
		s.mu.Unlock()
	}()
	writeJSON(w, map[string]any{"ok": true, "job_id": jobID})
}

func (s *Server) handleSyncCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	s.mu.Lock()
	cancel, ok := s.jobCancel[jobID]
	if ok {
		delete(s.jobCancel, jobID)
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	cancel()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSyncJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	logs := make([]map[string]any, 0, len(s.logFiles))
	for id, path := range s.logFiles {
		info, err := os.Stat(path)
		logs = append(logs, map[string]any{
			"job_id": id,
			"log":    path,
			"size": func() int64 {
				if err == nil {
					return info.Size()
				}
				return 0
			}(),
			"ready": true,
		})
	}
	s.mu.Unlock()
	writeJSON(w, logs)
}

func (s *Server) handleSyncLog(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	s.mu.Lock()
	path, ok := s.logFiles[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "log not ready", http.StatusNotFound)
		return
	}
	// 浏览器请求时返回简易 HTML 页面，否则返回原始文本
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		data, _ := os.ReadFile(path)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>同步日志 %s</title>`, id)
		fmt.Fprintf(w, `<style>body{background:#0f1115;color:#e6e9ef;font-family:Consolas,monospace;padding:20px;white-space:pre-wrap;line-height:1.6}h1{color:#4c8bf5}</style></head><body>`)
		fmt.Fprintf(w, `<h1>GoSync 同步日志 · %s</h1>`, id)
		fmt.Fprintf(w, `<p><a href="/api/sync/log/download?job_id=%s" style="color:#4c8bf5">下载纯文本</a> · <a href="/" style="color:#4c8bf5">返回主页</a></p>`, id)
		fmt.Fprintf(w, `<pre>%s</pre>`, htmlEscape(string(data)))
		fmt.Fprintf(w, `</body></html>`)
		return
	}
	http.ServeFile(w, r, path)
}

func htmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Server) handleSyncLogDownload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	s.mu.Lock()
	path, ok := s.logFiles[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "log not ready", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=gosync-"+id+".log")
	http.ServeFile(w, r, path)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.SyncHub.Subscribe()
	defer cancel()
	// 初次发送 info
	fmt.Fprintf(w, "event: info\ndata: %s\n\n", mustJSON(map[string]any{
		"device_id":  s.DeviceID,
		"listen":     s.Cfg.Snapshot().Listen,
		"started_at": time.Now().Unix(),
	}))
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, mustJSON(ev))
			flusher.Flush()
		}
	}
}

// 对外暴露的端点（不需要鉴权，便于另一台设备直接拉取/推送）

func (s *Server) handlePeerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"id":   s.DeviceID,
		"name": s.Cfg.Snapshot().DeviceName,
	})
}

func (s *Server) handlePeerManifest(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	if !s.isRegisteredFolder(folder) {
		http.Error(w, "folder not registered", http.StatusForbidden)
		return
	}
	syncpkg.ReceiveManifest(w, r, folder)
}

func (s *Server) handlePeerFile(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	if !s.isRegisteredFolder(folder) {
		http.Error(w, "folder not registered", http.StatusForbidden)
		return
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	syncpkg.ReceiveFile(w, r, abs)
}

func (s *Server) isRegisteredFolder(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	for _, f := range s.Cfg.Snapshot().Folders {
		if f == abs {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newJobID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "job-" + hex.EncodeToString(b[:])
}

// writeJobLog 将一次同步任务的结果写入可读文本日志。
func writeJobLog(path string, result *syncpkg.JobResult, runErr error) error {
	if result == nil {
		result = &syncpkg.JobResult{}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := f
	started := time.Unix(result.Started, 0).Format(time.RFC3339)
	finished := time.Unix(result.Finished, 0).Format(time.RFC3339)
	fmt.Fprintf(w, "GoSync 同步日志\n")
	fmt.Fprintf(w, "任务 ID: %s\n", result.JobID)
	fmt.Fprintf(w, "对端:    %s\n", result.Peer)
	fmt.Fprintf(w, "文件夹:  %s\n", result.Folder)
	fmt.Fprintf(w, "开始:    %s\n", started)
	fmt.Fprintf(w, "结束:    %s\n", finished)
	if runErr != nil {
		fmt.Fprintf(w, "异常:    %s\n", runErr.Error())
	}
	fmt.Fprintf(w, "整体校验: %v\n\n", result.VerifyOK)
	fmt.Fprintf(w, "=== 成功 (%d) ===\n", len(result.Success))
	for _, fr := range result.Success {
		fmt.Fprintf(w, "  [OK]   %s  size=%d  sha256=%s\n", fr.Path, fr.Size, fr.Hash)
	}
	fmt.Fprintf(w, "\n=== 失败 (%d) ===\n", len(result.Failed))
	for _, fr := range result.Failed {
		fmt.Fprintf(w, "  [FAIL] %s  err=%s\n", fr.Path, fr.Error)
	}
	fmt.Fprintf(w, "\n=== 跳过 (%d) ===\n", len(result.Skipped))
	for _, fr := range result.Skipped {
		fmt.Fprintf(w, "  [SKIP] %s\n", fr.Path)
	}
	return nil
}

// helper 防止 strconv 引入单独 import
var _ = strconv.Itoa
