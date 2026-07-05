// Package sync 实现 P2P 文件夹同步的核心逻辑：
// 1. 在两端构建文件清单（SHA-256 + 大小 + 修改时间）
// 2. 对比后由发起端将缺失或被修改的文件推送到对端
// 3. 接收端在写盘时同步计算哈希并校验
// 4. 同步过程通过 Hub 实时广播进度事件
package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gosync/internal/folders"
)

// Manifest 是单方（本地或远端）的文件夹清单。
type Manifest struct {
	Folder  string             `json:"folder"`   // 文件夹绝对路径
	Files   []folders.FileInfo `json:"files"`    // 文件列表
	BuiltAt int64              `json:"built_at"` // 构建时间（Unix 秒）
}

// Diff 描述需要从本端推送到对端的文件。
type Diff struct {
	Upload []folders.FileInfo `json:"upload"` // 新增或修改的文件
}

// BuildManifest 扫描本地文件夹并构建清单。
func BuildManifest(root string) (Manifest, error) {
	files, err := folders.ListFiles(root, nil)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Folder:  root,
		Files:   files,
		BuiltAt: time.Now().Unix(),
	}, nil
}

// BuildRemoteManifest 请求对端清单。
// remoteFolder 是对端希望使用的文件夹绝对路径（两端路径可能不同，接收端会做归一化与自动注册）。
// 返回的错误会明确区分：404（远端目录在磁盘上不存在）、403（对端拒绝）、其它网络/HTTP 错误。
func BuildRemoteManifest(ctx context.Context, base, remoteFolder string) (Manifest, error) {
	url := strings.TrimRight(base, "/") + "/api/peer/manifest?folder=" + urlQuery(remoteFolder)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Manifest{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("请求对端清单失败（%s）：%w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return Manifest{}, fmt.Errorf("对端目录不存在且未注册：%s（请先在对端软件中创建该目录，或在 UI 中添加同步文件夹）", remoteFolder)
		case http.StatusForbidden:
			return Manifest{}, fmt.Errorf("对端拒绝访问目录：%s（%s）", remoteFolder, string(body))
		case http.StatusBadRequest:
			return Manifest{}, fmt.Errorf("对端认为请求参数无效：%s（%s）", remoteFolder, string(body))
		default:
			return Manifest{}, fmt.Errorf("对端返回 %s：%s（folder=%s）", resp.Status, string(body), remoteFolder)
		}
	}
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("解析对端清单失败：%w", err)
	}
	return m, nil
}

// ComputeDiff 对比两端清单，返回需要上传的文件。
// 判定规则：相对路径相同则比较 SHA-256；哈希不同视为修改，需重传。
func ComputeDiff(local, remote Manifest) Diff {
	remoteIdx := make(map[string]folders.FileInfo, len(remote.Files))
	for _, f := range remote.Files {
		remoteIdx[f.RelPath] = f
	}
	var out []folders.FileInfo
	for _, lf := range local.Files {
		rf, ok := remoteIdx[lf.RelPath]
		if !ok || rf.Hash != lf.Hash {
			out = append(out, lf)
		}
	}
	return Diff{Upload: out}
}

// Hub 将同步进度事件分发给所有订阅者（SSE）。
// subscribers 在并发场景下被读写（订阅/退订/广播），因此用 RWMutex 保护。
type Hub struct {
	mu          sync.RWMutex
	subscribers map[chan Event]struct{}
}

// Event 是同步过程的一条进度/状态消息。
type Event struct {
	Type      string  `json:"type"` // start/plan/progress/file_done/done/sync_error
	JobID     string  `json:"job_id"`
	Folder    string  `json:"folder,omitempty"`
	Peer      string  `json:"peer,omitempty"`
	File      string  `json:"file,omitempty"`
	Index     int     `json:"index,omitempty"`
	Total     int     `json:"total,omitempty"`
	BytesDone int64   `json:"bytes_done,omitempty"`
	BytesTot  int64   `json:"bytes_total,omitempty"`
	Speed     float64 `json:"speed_bps,omitempty"` // 字节/秒
	Status    string  `json:"status,omitempty"`    // success / failed / skipped
	Message   string  `json:"message,omitempty"`
	At        int64   `json:"at"`
}

// NewHub 构造 Hub。
func NewHub() *Hub {
	return &Hub{subscribers: make(map[chan Event]struct{})}
}

// Subscribe 订阅事件，返回只读 channel 和取消函数。
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
		}
		h.mu.Unlock()
		close(ch)
	}
}

// Publish 发送事件给所有订阅者。
func (h *Hub) Publish(e Event) {
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subscribers {
		select {
		case ch <- e:
		default:
			// 慢消费者丢弃，避免阻塞
		}
	}
}

// JobResult 是同步任务最终结果，供前端生成日志。
type JobResult struct {
	JobID    string       `json:"job_id"`
	Peer     string       `json:"peer"`
	Folder   string       `json:"folder"`
	Started  int64        `json:"started"`
	Finished int64        `json:"finished"`
	Success  []FileResult `json:"success"`
	Failed   []FileResult `json:"failed"`
	Skipped  []FileResult `json:"skipped"`
	VerifyOK bool         `json:"verify_ok"`
}

// FileResult 描述单个文件的同步结果。
type FileResult struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Hash   string `json:"hash"`
	Status string `json:"status"` // success / failed / skipped
	Error  string `json:"error,omitempty"`
}

// RunPush 发起一次推送同步任务。
// localRoot 本地文件夹；peerBase 对端 base URL；remoteFolder 对端希望写入的文件夹（必须已在对端注册）。
// hub 用于广播进度；jobID 自定义。
func RunPush(ctx context.Context, hub *Hub, jobID, localRoot, peerBase, remoteFolder string) (*JobResult, error) {
	hub.Publish(Event{Type: "start", JobID: jobID, Folder: localRoot, Peer: peerBase})
	result := &JobResult{
		JobID:   jobID,
		Peer:    peerBase,
		Folder:  localRoot,
		Started: time.Now().Unix(),
	}
	// 1. 构建本地清单
	local, err := BuildManifest(localRoot)
	if err != nil {
		hub.Publish(Event{Type: "sync_error", JobID: jobID, Message: "构建本地清单失败: " + err.Error()})
		return result, err
	}
	// 2. 拉取远端清单
	remote, err := BuildRemoteManifest(ctx, peerBase, remoteFolder)
	if err != nil {
		hub.Publish(Event{Type: "sync_error", JobID: jobID, Message: "拉取远端清单失败: " + err.Error()})
		return result, err
	}
	// 3. 计算差异
	diff := ComputeDiff(local, remote)
	total := len(diff.Upload)
	var totalBytes int64
	for _, f := range diff.Upload {
		totalBytes += f.Size
	}
	hub.Publish(Event{Type: "plan", JobID: jobID, Total: total, BytesTot: totalBytes, Message: fmt.Sprintf("需要上传 %d 个文件，共 %s", total, humanBytes(totalBytes))})
	if total == 0 {
		result.Finished = time.Now().Unix()
		result.VerifyOK = true
		hub.Publish(Event{Type: "done", JobID: jobID, Total: 0, Message: "无文件需要同步"})
		return result, nil
	}
	// 4. 逐个推送
	var (
		bytesDone int64
		winStart  = time.Now()
		winBytes  int64
	)
	for i, f := range diff.Upload {
		select {
		case <-ctx.Done():
			result.Finished = time.Now().Unix()
			hub.Publish(Event{Type: "sync_error", JobID: jobID, Message: "已取消"})
			return result, ctx.Err()
		default:
		}
		fr := FileResult{Path: f.RelPath, Size: f.Size, Hash: f.Hash}
		hashHex, written, err := pushFile(ctx, peerBase, localRoot, remoteFolder, f, func(n int64) {
			bytesDone += n
			winBytes += n
			elapsed := time.Since(winStart).Seconds()
			var speed float64
			if elapsed > 0 {
				speed = float64(winBytes) / elapsed
			}
			if elapsed > 1.5 {
				winStart = time.Now()
				winBytes = 0
			}
			hub.Publish(Event{
				Type:      "progress",
				JobID:     jobID,
				Index:     i + 1,
				Total:     total,
				BytesDone: bytesDone,
				BytesTot:  totalBytes,
				Speed:     speed,
				File:      f.RelPath,
			})
		})
		if err != nil {
			fr.Status = "failed"
			fr.Error = err.Error()
			result.Failed = append(result.Failed, fr)
			hub.Publish(Event{Type: "file_done", JobID: jobID, Index: i + 1, Total: total, File: f.RelPath, Status: "failed", Message: err.Error()})
			continue
		}
		if !strings.EqualFold(hashHex, f.Hash) {
			fr.Status = "failed"
			fr.Error = "对端校验失败"
			result.Failed = append(result.Failed, fr)
			hub.Publish(Event{Type: "file_done", JobID: jobID, Index: i + 1, Total: total, File: f.RelPath, Status: "failed", Message: "对端校验失败"})
			continue
		}
		fr.Status = "success"
		result.Success = append(result.Success, fr)
		hub.Publish(Event{
			Type:  "file_done",
			JobID: jobID,
			Index: i + 1, Total: total,
			File:   f.RelPath,
			Status: "success",
		})
		_ = written
	}
	// 5. 再次拉取远端清单校验整体一致性
	remote2, err := BuildRemoteManifest(ctx, peerBase, remoteFolder)
	if err == nil {
		idx := make(map[string]string, len(remote2.Files))
		for _, f := range remote2.Files {
			idx[f.RelPath] = f.Hash
		}
		ok := true
		for _, lf := range local.Files {
			if idx[lf.RelPath] != lf.Hash {
				ok = false
				break
			}
		}
		result.VerifyOK = ok
	}
	result.Finished = time.Now().Unix()
	hub.Publish(Event{
		Type:    "done",
		JobID:   jobID,
		Total:   total,
		Message: fmt.Sprintf("同步完成：成功 %d，失败 %d，校验 %v", len(result.Success), len(result.Failed), result.VerifyOK),
	})
	return result, nil
}

// pushFile 将单个文件推送到对端，边读边计算本地哈希。
// 同步时对端会再次计算哈希并比较。
// remoteFolder 是对端接收根目录（必须已在对端注册）。
func pushFile(ctx context.Context, peerBase, localRoot, remoteFolder string, f folders.FileInfo, onProgress func(int64)) (string, int64, error) {
	abs, err := folders.SafeJoin(localRoot, f.RelPath)
	if err != nil {
		return "", 0, err
	}
	src, err := os.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()

	pr, pw := io.Pipe()
	hasher := sha256.New()
	writer := io.MultiWriter(pw, hasher)

	// 后台读盘+哈希
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 64*1024)
		var total int64
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				if _, werr := writer.Write(buf[:n]); werr != nil {
					errCh <- werr
					return
				}
				total += int64(n)
				if onProgress != nil {
					onProgress(int64(n))
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					errCh <- pw.Close()
					return
				}
				errCh <- rerr
				return
			}
		}
	}()

	url := fmt.Sprintf("%s/api/peer/file?folder=%s&path=%s&expected_hash=%s",
		strings.TrimRight(peerBase, "/"),
		urlQuery(remoteFolder),
		urlQuery(f.RelPath),
		urlQuery(f.Hash),
	)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, pr)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = f.Size
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if rerr := <-errCh; rerr != nil && !errors.Is(rerr, io.EOF) {
		return "", 0, rerr
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("push %s: %s: %s", f.RelPath, resp.Status, string(body))
	}
	var ack struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(body, &ack); err != nil {
		return "", 0, err
	}
	return ack.Hash, f.Size, nil
}

// ReceiveFile 接收对端上传的文件：边写边算哈希，完成后与 expected_hash 校验。
// folder 为对端希望写入的根目录（必须已在本地注册为同步文件夹）。
func ReceiveFile(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	expected := r.URL.Query().Get("expected_hash")
	rel := r.URL.Query().Get("path")
	abs, err := folders.SafeJoin(root, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp := abs + ".gosync.tmp"
	dst, err := os.Create(tmp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := sha256.New()
	mw := io.MultiWriter(dst, h)
	n, err := io.Copy(mw, r.Body)
	dst.Close()
	if err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expected != "" && !strings.EqualFold(got, expected) {
		_ = os.Remove(tmp)
		http.Error(w, "hash mismatch", http.StatusBadRequest)
		return
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hash":    got,
		"bytes":   n,
		"path":    rel,
		"message": "ok",
	})
}

// ReceiveManifest 响应远端清单请求。
func ReceiveManifest(w http.ResponseWriter, r *http.Request, root string) {
	m, err := BuildManifest(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(m)
	_, _ = w.Write(buf.Bytes())
}

func urlQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteString("%20")
		case c == '/' || c == '\\':
			b.WriteByte('/')
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-' || c == '_' || c == '.' || c == '~' || c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func humanBytes(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	if n < k*k {
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	}
	if n < k*k*k {
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
}
