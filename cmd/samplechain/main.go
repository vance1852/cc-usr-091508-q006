package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"samplechain/internal/api"
	"samplechain/internal/seed"
	"samplechain/internal/store"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "HTTP 监听地址")
	dbPath := flag.String("db", envOr("DB_PATH", "samplechain.db"), "SQLite 数据库路径")
	seedFlag := flag.Bool("seed", false, "写入演示基础数据（班组/用户/方法/批次）")
	flag.Parse()

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	if *seedFlag {
		if _, err := seed.Load(db); err != nil {
			log.Fatalf("写入基础数据失败: %v", err)
		}
		log.Printf("基础数据已就绪")
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.NewRouter(db),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("样品链路服务监听 %s（数据库 %s）", *addr, *dbPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务失败: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("服务已停止")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
