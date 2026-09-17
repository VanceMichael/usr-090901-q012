// 协同止付工作单服务：纯后台 HTTP 服务，不连接银行核心或警务系统。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/usr/090901/q012/internal/config"
	"example.com/usr/090901/q012/internal/httpapi"
	"example.com/usr/090901/q012/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		listenAddr     = env("LISTEN_ADDR", ":8080")
		databaseURL    = env("DATABASE_URL", "postgres://app:local-dev-only@127.0.0.1:5432/app?sslmode=disable")
		fixturesDir    = env("FIXTURES_DIR", "./fixtures")
		faultInjection = os.Getenv("APP_FAULT_INJECTION") == "1"
	)

	fixtures, err := config.Load(fixturesDir)
	if err != nil {
		log.Fatalf("加载夹具失败: %v", err)
	}
	log.Printf("夹具规则版本 %s 已加载（时区 %s）", fixtures.Rules.Version, fixtures.Rules.Timezone)

	ctx := context.Background()
	var st *store.Store
	// 等待数据库就绪（容器编排下 postgres 可能慢于应用启动）。
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err = store.Connect(cctx, databaseURL)
		cancel()
		if err == nil {
			break
		}
		if attempt >= 30 {
			log.Fatalf("数据库连接失败: %v", err)
		}
		log.Printf("等待数据库就绪（第 %d 次）: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("迁移失败: %v", err)
	}

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           httpapi.New(fixtures, st, faultInjection),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if faultInjection {
		log.Printf("警告：故障注入已开启（仅供验收）")
	}

	go func() {
		log.Printf("监听 %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("正在退出…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
