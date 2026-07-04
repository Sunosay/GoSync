package device

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"runtime"
	"strings"
)

// ID 根据机器信息生成稳定且唯一的设备 ID。
// 使用主机名、MAC 地址、操作系统和架构作为输入，
// 同一台机器无论重启多少次都会得到同样的 ID。
func ID() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	mac := firstMAC()
	osName := runtime.GOOS
	arch := runtime.GOARCH
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	raw := strings.Join([]string{host, mac, osName, arch, user}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8]), nil
}

func firstMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "nomac"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		mac := iface.HardwareAddr.String()
		if mac != "" {
			return mac
		}
	}
	return "nomac"
}
