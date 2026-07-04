// Package discovery 实现局域网内的设备发现：
// 每个实例周期性通过 UDP 广播自己的 ID、名称和地址；
// 同时监听同一端口，把收到的心跳存入内存供 UI 查询。
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// Beacon 是设备对外广播的心跳包。
type Beacon struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`  // 形如 http://192.168.1.5:9000
	Folder   string `json:"folder"`   // 主同步文件夹名（展示用）
	SendTime int64  `json:"send_time"`
}

// Service 持有发现服务的运行状态。
type Service struct {
	mu        sync.RWMutex
	peers     map[string]Beacon // id -> 最新 beacon
	hubPort   int               // 广播/接收端口
	selfID    string
	selfName  string
	selfAddr  string
	selfFold  string
	conn      *net.UDPConn
	stopCh    chan struct{}
	OnUpdate  func(Beacon)
}

// New 构造发现服务（不启动）。
func New(hubPort int) *Service {
	return &Service{
		peers:   make(map[string]Beacon),
		hubPort: hubPort,
		stopCh:  make(chan struct{}),
	}
}

// SetSelf 设置本机信息。
func (s *Service) SetSelf(id, name, addr, folder string) {
	s.selfID = id
	s.selfName = name
	s.selfAddr = addr
	s.selfFold = folder
}

// Peers 拷贝当前已知对端列表。
func (s *Service) Peers() []Beacon {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Beacon, 0, len(s.peers))
	for _, b := range s.peers {
		if b.ID == s.selfID {
			continue
		}
		out = append(out, b)
	}
	return out
}

// Start 启动广播和监听 goroutine。
func (s *Service) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", s.hubPort))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	s.conn = conn
	go s.recvLoop(ctx)
	go s.broadcastLoop(ctx)
	go s.expireLoop(ctx)
	log.Printf("discovery: listening on UDP :%d", s.hubPort)
	return nil
}

// Stop 停止发现服务。
func (s *Service) Stop() {
	close(s.stopCh)
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *Service) broadcastLoop(ctx context.Context) {
	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: s.hubPort}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	payload := func() []byte {
		s.mu.RLock()
		defer s.mu.RUnlock()
		b := Beacon{ID: s.selfID, Name: s.selfName, Address: s.selfAddr, Folder: s.selfFold, SendTime: time.Now().Unix()}
		out, _ := json.Marshal(b)
		return out
	}
	// 立即广播一次，让 UI 更快看到自己
	if s.conn != nil {
		_, _ = s.conn.WriteToUDP(payload(), bcast)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			if s.conn == nil {
				continue
			}
			_, err := s.conn.WriteToUDP(payload(), bcast)
			if err != nil {
				log.Printf("discovery: broadcast: %v", err)
			}
		}
	}
}

func (s *Service) recvLoop(ctx context.Context) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var b Beacon
		if err := json.Unmarshal(buf[:n], &b); err != nil {
			continue
		}
		if b.ID == "" || b.ID == s.selfID {
			continue
		}
		// 如果对端没填地址，用本次收到包的源 IP 补全
		if b.Address == "" && src != nil {
			b.Address = fmt.Sprintf("http://%s", src.IP.String())
		}
		s.mu.Lock()
		old, exists := s.peers[b.ID]
		s.peers[b.ID] = b
		s.mu.Unlock()
		if !exists || old.Address != b.Address {
			if s.OnUpdate != nil {
				s.OnUpdate(b)
			}
		}
	}
}

func (s *Service) expireLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			cutoff := time.Now().Add(-15 * time.Second).Unix()
			s.mu.Lock()
			for id, b := range s.peers {
				if b.SendTime < cutoff {
					delete(s.peers, id)
				}
			}
			s.mu.Unlock()
		}
	}
}
