// 模型网关入口：单个二进制同时提供 /v1 网关接口、/api 管理接口和内嵌的 React 控制台。
package main

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"modelrelay/internal/gateway"
	"modelrelay/web"
)

// loadEnvFile 读取 KEY=VALUE 形式的私有配置文件（已存在的环境变量优先）。
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func main() {
	loadEnvFile(".env")
	if p := os.Getenv("PROVIDERS_ENV_FILE"); p != "" {
		loadEnvFile(p)
	}
	cfg := gateway.ConfigFromEnv()
	if sub, err := fs.Sub(web.Dist, "dist"); err == nil {
		if _, err := fs.Stat(sub, "index.html"); err == nil {
			cfg.WebFS = sub
		}
	}
	srv, err := gateway.New(cfg)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		log.Printf("模型网关已启动：http://localhost%s  数据库：%s", cfg.Addr, cfg.SQLitePath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，等待在途请求结束（最多 25 秒）…")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		// 仍未结束的请求由下次启动时的恢复路径处理（预留转待核实或释放）
		log.Printf("仍有请求未结束，强制关闭: %v", err)
		httpSrv.Close()
	}
	srv.Close()
}
