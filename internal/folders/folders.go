package folders

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileInfo 描述文件夹内的一个文件。
type FileInfo struct {
	RelPath string `json:"rel_path"` // 相对文件夹根的路径，使用正斜杠
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"` // 修改时间（Unix 秒）
	Hash    string `json:"hash"`   // SHA-256 十六进制
}

// Summary 描述一个同步文件夹的概览。
type Summary struct {
	Path     string `json:"path"`
	NumFiles int    `json:"num_files"`
	TotalSz  int64  `json:"total_size"`
}

// ScanSummary 扫描并汇总文件夹。
func ScanSummary(root string) (Summary, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Summary{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Summary{}, err
	}
	if !st.IsDir() {
		return Summary{}, fmt.Errorf("not a directory: %s", abs)
	}
	var sum Summary
	sum.Path = abs
	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum.NumFiles++
		sum.TotalSz += info.Size()
		return nil
	})
	return sum, err
}

// ListFiles 列出文件夹内全部文件，并计算 SHA-256。
// 进度通过可选的 progress 回调反馈（已处理字节数，1 表示一个文件完成）。
func ListFiles(root string, progress func(done int)) ([]FileInfo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []FileInfo
	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		hash, err := fileHash(p)
		if err != nil {
			return err
		}
		out = append(out, FileInfo{
			RelPath: rel,
			Size:    info.Size(),
			MTime:   info.ModTime().Unix(),
			Hash:    hash,
		})
		if progress != nil {
			progress(1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// ListFilesFast 仅列举文件元数据，不计算哈希。
func ListFilesFast(root string) ([]FileInfo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []FileInfo
	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, FileInfo{
			RelPath: rel,
			Size:    info.Size(),
			MTime:   info.ModTime().Unix(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// fileHash 计算单个文件的 SHA-256 哈希。
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashOf 计算并返回 SHA-256 字符串。
func HashOf(path string) (string, error) {
	return fileHash(path)
}

// SafeJoin 安全拼接子路径并阻止越界。
func SafeJoin(root, rel string) (string, error) {
	clean := filepath.FromSlash(rel)
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid path: %s", rel)
	}
	return filepath.Join(root, clean), nil
}
