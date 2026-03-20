package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Port             int
	Host             string
	APIKey           string
	LogLevel         string
	KiroDBPath       string
	IdcRefreshURL    string
	CodeWhispererURL string
	ProfileARN       string
	ModelMap         map[string]string
	DefaultModel     string
	Debug            bool
}

type tomlConfig struct {
	Port             int               `toml:"port"`
	Host             string            `toml:"host"`
	APIKey           string            `toml:"api_key"`
	LogLevel         string            `toml:"log_level"`
	KiroDBPath       string            `toml:"kiro_db_path"`
	IdcRefreshURL    string            `toml:"idc_refresh_url"`
	CodeWhispererURL string            `toml:"codewhisperer_url"`
	ProfileARN       string            `toml:"profile_arn"`
	ModelMap         map[string]string `toml:"model_map"`
	DefaultModel     string            `toml:"default_model"`
}

func defaultDBPath() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "kiro-cli", "data.sqlite3")
	}
	return filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3")
}

func defaultModelMap() map[string]string {
	return map[string]string{
		"claude-opus-4-6-1m":         "claude-opus-4.6-1m",
		"claude-opus-4-6.1m":         "claude-opus-4.6-1m",
		"claude-opus-4-6":            "claude-opus-4.6",
		"claude-opus-4.6":            "claude-opus-4.6",
		"claude-sonnet-4-6":          "claude-sonnet-4.6",
		"claude-sonnet-4.6":          "claude-sonnet-4.6",
		"claude-sonnet-4-6-1m":       "claude-sonnet-4.6-1m",
		"claude-sonnet-4.6-1m":       "claude-sonnet-4.6-1m",
		"claude-opus-4.5":            "claude-opus-4.5",
		"claude-opus-4-5":            "claude-opus-4.5",
		"claude-opus-4-5-20251101":   "claude-opus-4.5-20251101",
		"claude-sonnet-4.5":          "claude-sonnet-4.5",
		"claude-sonnet-4.5-1m":       "claude-sonnet-4.5-1m",
		"claude-sonnet-4-5":          "claude-sonnet-4.5",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5-20250929",
		"claude-haiku-4.5":           "claude-haiku-4.5",
		"claude-haiku-4-5":           "claude-haiku-4.5",
		"claude-haiku-4-5-20251001":  "claude-haiku-4.5-20251001",
	}
}

func Load() *Config {
	// Load TOML file if exists
	var fileCfg tomlConfig
	if _, err := os.Stat("config.toml"); err == nil {
		toml.DecodeFile("config.toml", &fileCfg)
	}

	get := func(envKey, fileVal, def string) string {
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		if fileVal != "" {
			return fileVal
		}
		return def
	}

	getInt := func(envKey string, fileVal int, def int) int {
		if v := os.Getenv(envKey); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		if fileVal != 0 {
			return fileVal
		}
		return def
	}

	modelMap := defaultModelMap()
	if len(fileCfg.ModelMap) > 0 {
		for k, v := range fileCfg.ModelMap {
			modelMap[k] = v
		}
	}

	return &Config{
		Port:             getInt("PORT", fileCfg.Port, 8001),
		Host:             get("HOST", fileCfg.Host, "0.0.0.0"),
		APIKey:           get("API_KEY", fileCfg.APIKey, ""),
		LogLevel:         get("LOG_LEVEL", fileCfg.LogLevel, "info"),
		KiroDBPath:       get("KIRO_DB_PATH", fileCfg.KiroDBPath, defaultDBPath()),
		IdcRefreshURL:    get("IDC_REFRESH_URL", fileCfg.IdcRefreshURL, "https://oidc.us-east-1.amazonaws.com/token"),
		CodeWhispererURL: get("CODEWHISPERER_URL", fileCfg.CodeWhispererURL, "https://q.us-east-1.amazonaws.com/generateAssistantResponse"),
		ProfileARN:       get("PROFILE_ARN", fileCfg.ProfileARN, ""),
		ModelMap:         modelMap,
		DefaultModel:     get("DEFAULT_MODEL", fileCfg.DefaultModel, "claude-opus-4-6"),
	}
}
