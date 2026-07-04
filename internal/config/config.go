package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Config 保存程序的全部持久化状态。
type Config struct {
	Listen      string   `json:"listen"`       // 本机监听地址，例如 ":9000"
	DeviceName  string   `json:"device_name"`  // 设备名（仅展示用）
	Folders     []string `json:"folders"`      // 已添加的同步文件夹
	Peers       []Peer   `json:"peers"`        // 已知对端（ID -> Address）
	DiscoveryOn bool     `json:"discovery_on"` // 是否启用 LAN 广播发现
}

// Peer 表示一个已连接或已知的对端设备。
type Peer struct {
	ID      string `json:"id"`      // 设备 ID
	Name    string `json:"name"`    // 对端展示名
	Address string `json:"address"` // 对端访问地址，例如 "http://192.168.1.5:9000"
}

const defaultFile = "gosync.json"

// Store 提供线程安全的配置读写。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Load 从默认位置加载配置，文件不存在则使用默认值。
// 如果设置了 GOSYNC_CONFIG 环境变量，则从该路径加载。
func Load() (*Store, error) {
	if p := os.Getenv("GOSYNC_CONFIG"); p != "" {
		return loadFrom(p)
	}
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	return loadFrom(path)
}

func loadFrom(path string) (*Store, error) {
	s := &Store{path: path, cfg: Config{
		Listen:      ":9000",
		DiscoveryOn: true,
	}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, err
	}
	return s, nil
}

func configPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return defaultFile, nil
	}
	return filepath.Join(filepath.Dir(exe), defaultFile), nil
}

// Snapshot 返回当前配置的深拷贝。
func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cfg
	out.Folders = append([]string(nil), s.cfg.Folders...)
	peers := make([]Peer, len(s.cfg.Peers))
	copy(peers, s.cfg.Peers)
	out.Peers = peers
	return out
}

// AddFolder 添加一个同步文件夹（去重）。
func (s *Store) AddFolder(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.cfg.Folders {
		if f == abs {
			return nil
		}
	}
	s.cfg.Folders = append(s.cfg.Folders, abs)
	return s.saveLocked()
}

// RemoveFolder 移除一个同步文件夹。
func (s *Store) RemoveFolder(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.cfg.Folders[:0]
	for _, f := range s.cfg.Folders {
		if f != abs {
			out = append(out, f)
		}
	}
	s.cfg.Folders = out
	return s.saveLocked()
}

// UpsertPeer 添加或更新对端信息。
func (s *Store) UpsertPeer(p Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ex := range s.cfg.Peers {
		if ex.ID == p.ID {
			s.cfg.Peers[i] = p
			return s.saveLocked()
		}
	}
	s.cfg.Peers = append(s.cfg.Peers, p)
	return s.saveLocked()
}

// RemovePeer 按 ID 移除对端。
func (s *Store) RemovePeer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.cfg.Peers[:0]
	for _, p := range s.cfg.Peers {
		if p.ID != id {
			out = append(out, p)
		}
	}
	s.cfg.Peers = out
	return s.saveLocked()
}

// SetListen 修改监听地址。
func (s *Store) SetListen(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Listen = addr
	return s.saveLocked()
}

// SetDeviceName 修改本机展示名。
func (s *Store) SetDeviceName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.DeviceName = name
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}
