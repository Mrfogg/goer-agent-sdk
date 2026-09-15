package main

import (
	"errors"
	"log"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	loadDotEnv()

	cfg := ConfigFromEnv()

	// 教学时想看 Gin 每个请求的日志，把下面这行注释掉即可（默认是 debug 模式）。
	gin.SetMode(gin.ReleaseMode)

	if cfg.Token == "" {
		log.Println("[提示] 没有设置 LLM_TOKEN，调用模型时会失败。例：export LLM_TOKEN=sk-xxx")
	}

	router := newRouter(cfg)
	log.Printf("lesson-01 已启动：%s", visitURL(cfg.Addr))
	if err := router.Run(cfg.Addr); err != nil {
		log.Fatal(err)
	}
}

// visitURL 把监听地址变成能直接点开的地址：":8080" → "http://localhost:8080"。
func visitURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}

// loadDotEnv 读取环境变量文件，默认是当前目录的 .env（可以用 ENV_FILE 换路径）。
//
// godotenv.Load 不会覆盖已经存在的环境变量，所以优先级天然是：
// 真实环境变量 > .env。本地开发把 key 写在 .env 里，线上直接给环境变量，两边互不干扰。
func loadDotEnv() {
	path := envOrDefault("ENV_FILE", ".env")

	switch err := godotenv.Load(path); {
	case err == nil:
		log.Printf("已加载环境变量文件：%s", path)
	case errors.Is(err, os.ErrNotExist):
		log.Printf("没有找到 %s，直接用系统的环境变量", path)
	default:
		log.Printf("[警告] 读取 %s 失败：%v", path, err)
	}
}
