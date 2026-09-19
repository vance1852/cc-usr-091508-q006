// samplechain 服务入口：装载配置、打开 SQLite、启动 Chi HTTP 服务。
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"samplechain/internal/api"
	"samplechain/internal/store"
)

func main() {
	dsn := env("SAMPLECHAIN_DB", "samplechain.db")
	addr := env("SAMPLECHAIN_ADDR", ":8080")

	st, err := store.Open(dsn)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	if err := store.Seed(st); err != nil {
		log.Fatalf("种子数据失败: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewServer(st),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("样品链路服务已启动：%s（数据库 %s）", addr, dsn)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf(err.Error())
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
