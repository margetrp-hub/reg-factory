// Command regd 是 reg-factory 的 Go 控制面：任务队列、日志流、资产与配置 API，
// 同时托管 Next.js 静态前端。
package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server"
	"github.com/tiantianGPU/reg-factory/server/internal/api"
	"github.com/tiantianGPU/reg-factory/server/internal/apps/mail"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/nodeserver"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := settings.Load()
	// /data/.env 里可被面板编辑的配置参与启动（真实环境变量优先）
	if n := settings.LoadEnvFile(cfg.EnvFile); n > 0 {
		log.Printf("[regd] 从 %s 注入 %d 项配置", cfg.EnvFile, n)
		cfg = settings.Load()
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("[regd] 配置校验失败: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("[regd] 数据目录不可用: %v", err)
	}

	// 打印生效的 DSN（脱敏），便于确认 PG/SQLite 分流是否正确
	log.Printf("[regd] 数据库后端: %s", store.DialectOf(cfg.DBPath))
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[regd] 打开数据库失败: %v", err)
	}
	defer st.Close()
	previousLog := log.Writer()
	log.SetOutput(io.MultiWriter(previousLog, &store.RuntimeLog{Store: st}))
	defer log.SetOutput(previousLog)
	// Inventory relay needs the version/status projection even when this is an
	// existing SG database created before the relay was introduced.
	if err := st.EnsureInventoryRelay(); err != nil {
		log.Fatalf("[regd] 账号库 relay 迁移失败: %v", err)
	}

	if err := runner.RecoverCredentials(cfg, st); err != nil {
		log.Fatalf("[regd] 原凭据任务收尾失败: %v", err)
	}
	if recovered, err := st.RecoverInterrupted(); err == nil && recovered > 0 {
		log.Printf("[regd] 上次退出遗留 %d 个任务已标记为 interrupted", recovered)
	}

	env := settings.NewEnv(cfg.EnvFile)
	manager := runner.New(cfg, st)
	authenticator := auth.New(cfg)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(requestLogger())

	// 统一资产库：把旧路径（creds/ cookies/ tokens/）搬进 /data/assets/
	if moved, err := assetstore.New(cfg.DataDir).Migrate(cfg.DataDir); err != nil {
		log.Printf("[regd] 资产迁移失败: %v", err)
	} else if moved > 0 {
		log.Printf("[regd] 已迁移 %d 个资产到 /data/assets/", moved)
	}

	assetSyncLoop(cfg.DataDir)

	version := readVersion(cfg)
	// IC 邮箱 app（同仓同进程）：/api/apps/mail/* 与 /apps/mail 页面
	icApp, err := mail.New(mail.NewConfigFromEnv(cfg.DataDir))
	if err != nil {
		log.Printf("[regd] IC 邮箱初始化失败（跳过该模块）: %v", err)
	} else {
		defer icApp.Close()
		icApp.Start(context.Background())
		defer icApp.Stop()
		log.Printf("[regd] IC 邮箱已挂载: /api/apps/mail")
	}

	apiServer := api.New(cfg, env, st, manager, authenticator, version, icApp)
	apiServer.Register(router)
	checkContext, stopChecks := context.WithCancel(context.Background())
	checksDone := apiServer.StartCredentialChecks(checkContext)
	defer stopChecks()
	if synced, syncErr := apiServer.SeedAccounts(); syncErr != nil {
		log.Printf("[regd] 账号主数据同步失败: %v", syncErr)
	} else {
		log.Printf("[regd] 账号主数据已同步 %d 条", synced)
	}
	apiServer.StartLivenessLoop(context.Background())
	productionNode := nodeserver.New(nodeserver.Config{
		Enabled:     cfg.NodeID != "" && len(cfg.NodeKey) >= 32,
		NodeID:      cfg.NodeID,
		Key:         []byte(cfg.NodeKey),
		ReceiptKey:  []byte(cfg.NodeReceiptKey),
		BusinessKey: []byte(cfg.NodeBusinessKey),
		ArtifactKey: []byte(cfg.NodeArtifactKey),
		Platform:    cfg.NodePlatform,
	}, manager, st)
	productionNode.Register(router)
	productionContext, stopProduction := context.WithCancel(context.Background())
	defer stopProduction()
	productionNode.StartProduction(productionContext)

	registerStatic(router)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("[regd] reg-factory Go 控制面 v%s", version)
	log.Printf("[regd] 监听 %s | 数据目录 %s | 环境文件 %s", cfg.Addr, cfg.DataDir, cfg.EnvFile)
	log.Printf("[regd] 并发上限 %d | 内存闸门 %0.f%% | Python %s", cfg.MaxParallel, cfg.GuardPct, cfg.Python)
	log.Printf("[regd] 内存水位 %+v", manager.Memory())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[regd] 监听失败: %v", err)
		}
	}()

	<-stop
	log.Printf("[regd] 收到退出信号，停止任务并关闭")
	stopChecks()
	<-checksDone
	manager.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func readVersion(cfg *settings.Settings) string {
	for _, candidate := range []string{
		filepath.Join(cfg.AppRoot, "VERSION"),
		filepath.Join(cfg.DataDir, "VERSION"),
	} {
		if raw, err := os.ReadFile(candidate); err == nil {
			text := strings.TrimSpace(string(raw))
			if text != "" {
				return text
			}
		}
	}
	return "dev"
}

// requestLogger 只记录 API 与非静态请求，避免刷屏。
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.Request.URL.Path
		if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/internal/work/") {
			return
		}
		if path == "/api/health" || path == "/api/memory" || path == "/api/system" ||
			path == "/api/logs" || path == "/api/logs/stream" || strings.HasSuffix(path, "/stream") {
			return
		}
		log.Printf("[api] %s %s %d %dms", c.Request.Method, path, c.Writer.Status(), time.Since(start).Milliseconds())
	}
}

// registerStatic 托管 Next.js 静态导出，并对未知路径回落 index.html。
func registerStatic(router *gin.Engine) {
	sub, err := fs.Sub(server.WebFS, "webdist")
	if err != nil {
		log.Printf("[regd] 前端资源不可用（仅 API 模式）: %v", err)
		return
	}
	fileServer := http.FileServer(http.FS(sub))

	router.NoRoute(func(c *gin.Context) {
		reqPath := c.Request.URL.Path
		if strings.HasPrefix(reqPath, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "path": reqPath})
			return
		}
		clean := strings.TrimPrefix(path.Clean(reqPath), "/")
		for _, candidate := range []string{clean, path.Join(clean, "index.html"), clean + ".html"} {
			if candidate == "" {
				continue
			}
			info, statErr := fs.Stat(sub, candidate)
			if statErr != nil || info.IsDir() {
				continue
			}
			// http.FileServer 会把 /index.html 301 到 ./，这里直接吐内容，
			// 保证 / 和 /tasks/ 都返回 200。
			if strings.HasSuffix(candidate, ".html") {
				raw, readErr := fs.ReadFile(sub, candidate)
				if readErr != nil {
					continue
				}
				c.Data(http.StatusOK, "text/html; charset=utf-8", raw)
				return
			}
			c.Request.URL.Path = "/" + candidate
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		// 文件确实不存在：/_next/ 下的构建产物必须返回 404。
		// 回落成 index.html 会让浏览器把 HTML 当 JS/CSS 解析，交互全部失效且极难排查。
		if strings.HasPrefix(reqPath, "/_next/") {
			c.Status(http.StatusNotFound)
			return
		}
		index, openErr := sub.Open("index.html")
		if openErr != nil {
			c.String(http.StatusNotFound, "前端未构建：webdist/index.html 缺失")
			return
		}
		defer index.Close()
		raw, _ := io.ReadAll(index)
		c.Data(http.StatusOK, "text/html; charset=utf-8", raw)
	})
}

// assetSyncLoop 每 60s 把旧路径新产出的资产增量搬进 /data/assets/。
// Python 执行器仍按老路径落盘，这里负责收敛，避免两边各写一份。
func assetSyncLoop(dataDir string) {
	store := assetstore.New(dataDir)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if moved, err := store.Migrate(dataDir); err == nil && moved > 0 {
				log.Printf("[regd] 增量迁移 %d 个资产到 /data/assets/", moved)
			}
		}
	}()
}

// maskDSN 脱敏 DSN 里的密码。
func maskDSN(dsn string) string {
	if i := strings.Index(dsn, "://"); i > 0 {
		rest := dsn[i+3:]
		if at := strings.Index(rest, "@"); at > 0 {
			if colon := strings.Index(rest[:at], ":"); colon > 0 {
				return dsn[:i+3] + rest[:colon+1] + "***" + rest[at:]
			}
		}
	}
	if strings.Contains(dsn, "password=") {
		parts := strings.Split(dsn, "password=")
		if len(parts) == 2 {
			end := strings.IndexAny(parts[1], " 	")
			if end < 0 {
				return parts[0] + "password=***"
			}
			return parts[0] + "password=***" + parts[1][end:]
		}
	}
	return dsn
}
