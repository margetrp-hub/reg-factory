package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/egress"
)

// ProductionProxy selects an existing connection. It never rotates a shared
// proxy or copies credentials back into configuration.
func ProductionProxy(env []string, appRoot string) (string, error) {
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = strings.TrimSpace(value)
	}
	switch values["REG_FACTORY_PRODUCTION_PROXY_SOURCE"] {
	case "", "custom":
		return values["REG_FACTORY_PRODUCTION_PROXY"], nil
	case "direct":
		return "", nil
	case "clash":
		if proxy, exists := values["CLASH_PROXY"]; exists {
			if proxy == "" {
				return "", fmt.Errorf("共享 Clash 未配置代理地址")
			}
			return proxy, nil
		}
		return "http://127.0.0.1:7897", nil
	case "residential":
		proxy := values["REG_FACTORY_PROXY"]
		pool := strings.FieldsFunc(values["REG_FACTORY_PROXY_POOL"], func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r'
		})
		var proxies []string
		for _, item := range pool {
			if value := strings.TrimSpace(item); value != "" {
				proxies = append(proxies, value)
			}
		}
		if len(proxies) > 0 {
			state := values["REG_FACTORY_PROXY_STATE_FILE"]
			if state == "" {
				state = filepath.Join(appRoot, "runtime", "state", "residential_proxy_index.txt")
			}
			// Match the residential pool's initial/current index; this is a read,
			// not the provider-wide rotate operation.
			raw, _ := os.ReadFile(state)
			index := atoiDefault(strings.TrimSpace(string(raw)), 0)
			proxy = proxies[(index%len(proxies)+len(proxies))%len(proxies)]
		}
		if proxy == "" {
			return "", fmt.Errorf("共享住宅代理未配置地址或代理池")
		}
		parsed, err := egress.ParseProxyURL(proxy)
		if err != nil {
			return "", fmt.Errorf("共享住宅代理地址格式无效")
		}
		return parsed.URL, nil
	default:
		return "", fmt.Errorf("生产与凭据修复请选择支持的出口")
	}
}

// BindProductionProxy freezes the selected endpoint for the whole worker,
// including browser login, token exchange and result verification.
func BindProductionProxy(env []string, appRoot string) ([]string, error) {
	proxy, err := ProductionProxy(env, appRoot)
	if err != nil {
		return nil, err
	}
	bound := make([]string, 0, len(env)+2)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "REG_FACTORY_PRODUCTION_PROXY" && key != "REG_FACTORY_PRODUCTION_PROXY_SOURCE" {
			bound = append(bound, entry)
		}
	}
	return append(bound, "REG_FACTORY_PRODUCTION_PROXY_SOURCE=custom", "REG_FACTORY_PRODUCTION_PROXY="+proxy), nil
}

func (e *Env) ProductionProxy(appRoot string) (string, error) {
	env, err := WorkerEnvironment(e.path)
	if err != nil {
		return "", err
	}
	return ProductionProxy(env, appRoot)
}
