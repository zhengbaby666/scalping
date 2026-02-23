package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 全局配置
type Config struct {
	// Apex API 配置
	API APIConfig `yaml:"api"`

	// 交易对，例如 BTC-USDC
	Symbol string `yaml:"symbol"`

	// 策略参数
	Strategy StrategyConfig `yaml:"strategy"`

	// 风控参数
	RiskControl RiskConfig `yaml:"risk_control"`
}

// APIConfig Apex Pro REST/WS 接口配置
type APIConfig struct {
	BaseURL    string `yaml:"base_url"`   // REST 基础地址
	WsURL      string `yaml:"ws_url"`     // WebSocket 地址
	APIKey     string `yaml:"api_key"`    // API Key
	APISecret  string `yaml:"api_secret"` // API Secret
	Passphrase string `yaml:"passphrase"` // 口令（部分接口需要）
	L2Key      string `yaml:"l2_key"`     // StarkEx L2 私鑰（Apex Pro 需要）

	// WebSocket 重连配置
	WsMaxReconnectIntervalSec int `yaml:"ws_max_reconnect_interval_sec"` // 最大退避间隔（秒，默认 30）
	WsPingIntervalSec         int `yaml:"ws_ping_interval_sec"`          // Ping 间隔（秒，默认 20）
	WsPongTimeoutSec          int `yaml:"ws_pong_timeout_sec"`           // Pong 超时（秒，默认 10）
}

// StrategyConfig 剥皮头策略参数
type StrategyConfig struct {
	// 挂单价差（相对于中间价的 tick 数）
	SpreadTicks int `yaml:"spread_ticks"`

	// 单笔下单数量（合约张数）
	OrderSize float64 `yaml:"order_size"`

	// 最大持仓量（合约张数，超过则停止开仓）
	MaxPosition float64 `yaml:"max_position"`

	// 订单刷新间隔（毫秒）
	RefreshIntervalMs int `yaml:"refresh_interval_ms"`

	// 盈利目标（USDC，达到后撤单平仓）
	TakeProfitUSDC float64 `yaml:"take_profit_usdc"`

	// 止损（USDC，超过后撤单平仓）
	StopLossUSDC float64 `yaml:"stop_loss_usdc"`

	// 最小价差阈值（低于此值不挂单，避免手续费亏损）
	MinSpreadUSDC float64 `yaml:"min_spread_usdc"`

	// 价格精度（小数位数）
	PricePrecision int `yaml:"price_precision"`

	// 数量精度（小数位数）
	SizePrecision int `yaml:"size_precision"`
}

// RiskConfig 风控配置
type RiskConfig struct {
	// 单日最大亏损（USDC）
	MaxDailyLossUSDC float64 `yaml:"max_daily_loss_usdc"`

	// 最大连续亏损次数
	MaxConsecutiveLoss int `yaml:"max_consecutive_loss"`

	// 账户最低余额（USDC，低于此值停止交易）
	MinBalanceUSDC float64 `yaml:"min_balance_usdc"`
}

// Load 从 YAML 文件加载配置，支持环境变量覆盖
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// 环境变量优先级高于配置文件
	if v := os.Getenv("APEX_API_KEY"); v != "" {
		cfg.API.APIKey = v
	}
	if v := os.Getenv("APEX_API_SECRET"); v != "" {
		cfg.API.APISecret = v
	}
	if v := os.Getenv("APEX_PASSPHRASE"); v != "" {
		cfg.API.Passphrase = v
	}
	if v := os.Getenv("APEX_L2_KEY"); v != "" {
		cfg.API.L2Key = v
	}

	return cfg, nil
}
