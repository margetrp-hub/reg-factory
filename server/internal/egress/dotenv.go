package egress

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// 为什么需要这个
// ------------
// Python 侧的 config.py 启动时会主动 _load_dotenv() 把 /data/.env 读进
// os.environ，所以 register_*.py 能拿到里面的配置。
//
// Go 的 os.Getenv 只读进程环境变量，而 entrypoint-go.sh 只导出了
// 少数几个 key，/data/.env 里的绝大多数配置（包括 PLUS_CARD_*）
// 根本不在进程 env 里。
//
// 结果就是 ResolveSecret("env:PLUS_CARD_NUMBER") 永远返回空。
// 这个文件解决这件事：启动时把 /data/.env 读进来，但不覆盖已有的
// 进程环境变量（容器 env 优先，与 Python 侧行为一致）。

var (
	dotenvOnce sync.Once
	dotenvMu   sync.RWMutex
	dotenvKeys int
	dotenvPath string
	dotenvErr  error
)

// DefaultDotenvPath 是默认的 .env 位置。
const DefaultDotenvPath = "/data/.env"

// LoadDotenv 读 .env 文件并注入进程环境变量。
//
// 规则与 Python 的 _load_dotenv 一致：
//   - 只解析 KEY=VALUE
//   - 忽略空行和 # 注释
//   - 值两端的引号会被去掉
//   - 已经存在于环境变量里的 key 不覆盖
//
// 多次调用只生效第一次（幂等）。
func LoadDotenv(path string) (int, error) {
	dotenvOnce.Do(func() {
		if path == "" {
			path = DefaultDotenvPath
		}
		dotenvPath = path

		f, err := os.Open(path)
		if err != nil {
			dotenvErr = err
			return
		}
		defer f.Close()

		n := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			val = strings.TrimSpace(val)
			val = strings.Trim(val, `"'`)

			// 容器 env 优先，不覆盖
			if _, exists := os.LookupEnv(key); exists {
				continue
			}
			if err := os.Setenv(key, val); err == nil {
				n++
			}
		}
		if err := sc.Err(); err != nil {
			dotenvErr = err
		}

		dotenvMu.Lock()
		dotenvKeys = n
		dotenvMu.Unlock()
	})

	dotenvMu.RLock()
	defer dotenvMu.RUnlock()
	return dotenvKeys, dotenvErr
}

// DotenvStatus 返回加载状态，供自检使用。
func DotenvStatus() map[string]any {
	dotenvMu.RLock()
	defer dotenvMu.RUnlock()
	return map[string]any{
		"path":        dotenvPath,
		"loaded_keys": dotenvKeys,
		"error":       errString(dotenvErr),
	}
}

// ResetDotenvForTest 清掉 once，只给测试用。
func ResetDotenvForTest() {
	dotenvOnce = sync.Once{}
	dotenvMu.Lock()
	dotenvKeys = 0
	dotenvPath = ""
	dotenvErr = nil
	dotenvMu.Unlock()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// init 阶段自动加载默认 .env。
//
// 放在 init 而不是显式调用，是为了让所有使用 plus 包的程序
// （pluscheck / plusbind / regd）都不用记得加载这一步。
// 文件不存在时静默失败 —— 没配 .env 的环境也能正常启动。
func init() {
	_, _ = LoadDotenv(DefaultDotenvPath)
}
