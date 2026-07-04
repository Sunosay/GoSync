package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gosync/internal/api"
	"gosync/internal/config"
	"gosync/internal/device"
	"gosync/internal/discovery"
	syncpkg "gosync/internal/sync"
)

//go:embed web/*
var webFS embed.FS

func main() {
	addr := flag.String("addr", "", "监听地址，覆盖配置 (例如 :9000)")
	noWeb := flag.Bool("no-embed", false, "禁用内嵌静态资源，改用 ./web 目录")
	dataDir := flag.String("data", "", "数据目录，覆盖默认（存放配置和日志）")
	configPath := flag.String("config", "", "配置文件路径，覆盖默认")
	overrideID := flag.String("device-id", "", "覆盖自动生成的设备 ID（用于多实例测试）")
	overrideName := flag.String("device-name", "", "覆盖设备名")
	discoveryPort := flag.Int("discovery-port", 47811, "LAN 发现使用的 UDP 端口")
	noDiscovery := flag.Bool("no-discovery", false, "禁用 LAN 自动发现")
	flag.Parse()

	if *configPath != "" {
		_ = os.Setenv("GOSYNC_CONFIG", *configPath)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *dataDir != "" {
		if err := os.MkdirAll(*dataDir, 0o755); err != nil {
			log.Fatalf("data dir: %v", err)
		}
		_ = os.Setenv("GOSYNC_DATA_DIR", *dataDir)
		if *configPath == "" {
			_ = os.Setenv("GOSYNC_CONFIG", filepath.Join(*dataDir, "gosync.json"))
			cfg, err = config.Load()
			if err != nil {
				log.Fatalf("reload config: %v", err)
			}
		}
	}
	if *addr != "" {
		if err := cfg.SetListen(*addr); err != nil {
			log.Fatalf("set listen: %v", err)
		}
	}
	if *overrideName != "" {
		_ = cfg.SetDeviceName(*overrideName)
	}
	dataDirFinal := *dataDir
	if dataDirFinal == "" {
		dataDirFinal = filepath.Dir(defaultConfigPath())
	}
	logDir := filepath.Join(dataDirFinal, "logs")
	id, err := device.ID()
	if err != nil {
		log.Fatalf("device id: %v", err)
	}
	if *overrideID != "" {
		id = *overrideID
	}
	hub := syncpkg.NewHub()
	var disc *discovery.Service
	if !*noDiscovery {
		disc = discovery.New(*discoveryPort)
		disc.SetSelf(id, cfg.Snapshot().DeviceName, "http://"+localIP()+":"+portOf(cfg.Snapshot().Listen), primaryFolder(cfg))
	}
	apiServer, err := api.NewServerWithID(cfg, hub, disc, logDir, id)
	if err != nil {
		log.Fatalf("api server: %v", err)
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	if disc != nil {
		if err := disc.Start(rootCtx); err != nil {
			log.Printf("discovery start: %v (continuing without LAN discovery)", err)
		}
		defer disc.Stop()
	}

	mux := http.NewServeMux()
	apiServer.Routes(mux)
	staticFS, err := staticFileSystem(*noWeb)
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	mux.Handle("/", http.FileServer(staticFS))

	srv := &http.Server{
		Addr:              cfg.Snapshot().Listen,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		log.Printf("GoSync listening on http://%s (device id: %s)", srv.Addr, id)
		log.Printf("Open the URL above in your browser.")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	rootCancel()
}

func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "gosync.json"
	}
	return filepath.Join(filepath.Dir(exe), "gosync.json")
}

func portOf(addr string) string {
	addr = strings.TrimPrefix(addr, ":")
	if !strings.Contains(addr, ":") {
		return addr
	}
	_, p, err := net.SplitHostPort(addr)
	if err == nil {
		return p
	}
	return "9000"
}

func localIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			return ip4.String()
		}
	}
	return "127.0.0.1"
}

func primaryFolder(cfg *config.Store) string {
	folders := cfg.Snapshot().Folders
	if len(folders) == 0 {
		return ""
	}
	return folders[0]
}

func staticFileSystem(disableEmbed bool) (http.FileSystem, error) {
	if disableEmbed {
		return http.Dir("web"), nil
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	return http.FS(sub), nil
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// 避免导入未使用
var _ = strconv.Itoa
