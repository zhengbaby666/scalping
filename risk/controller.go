package risk

import (
	"fmt"
	"log"
	"sync"
	"time"

	"apex-scalping/config"
)

// TradeResult 单笔交易结果
type TradeResult struct {
	Pnl  float64 // 已实现盈亏（USDC，正=盈利，负=亏损）
	Side string  // BUY / SELL
}

// Controller 风控控制器
type Controller struct {
	cfg config.RiskConfig
	mu  sync.Mutex

	// 累计盈亏
	totalPnl    float64
	dailyPnl    float64
	dailyReset  time.Time // 当日重置时间

	// 连续亏损计数
	consecutiveLoss int

	// 是否已触发熔断（触发后需人工重置）
	halted    bool
	haltReason string
}

// NewController 创建风控控制器
func NewController(cfg config.RiskConfig) *Controller {
	now := time.Now()
	return &Controller{
		cfg:        cfg,
		dailyReset: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()),
	}
}

// RecordTrade 记录一笔成交，更新盈亏和连续亏损计数
func (c *Controller) RecordTrade(result TradeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maybeResetDaily()

	c.totalPnl += result.Pnl
	c.dailyPnl += result.Pnl

	if result.Pnl < 0 {
		c.consecutiveLoss++
		log.Printf("[风控] 亏损交易 pnl=%.4f USDC，连续亏损次数=%d", result.Pnl, c.consecutiveLoss)
	} else {
		// 盈利则重置连续亏损计数
		c.consecutiveLoss = 0
	}

	// 记录后立即检查是否需要熔断
	c.checkAndHalt()
}

// Check 在每次下单前调用，返回 error 表示风控拦截
func (c *Controller) Check(availableBalance float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maybeResetDaily()

	// 1. 熔断状态检查
	if c.halted {
		return fmt.Errorf("风控熔断中: %s", c.haltReason)
	}

	// 2. 账户可用余额检查
	if availableBalance < c.cfg.MinBalanceUSDC {
		reason := fmt.Sprintf("可用余额 %.4f USDC 低于最低限制 %.4f USDC", availableBalance, c.cfg.MinBalanceUSDC)
		c.halt(reason)
		return fmt.Errorf("[风控] %s", reason)
	}

	// 3. 单日亏损检查（dailyPnl 为负数时取绝对值比较）
	if -c.dailyPnl >= c.cfg.MaxDailyLossUSDC {
		reason := fmt.Sprintf("单日亏损 %.4f USDC 超过限制 %.4f USDC", -c.dailyPnl, c.cfg.MaxDailyLossUSDC)
		c.halt(reason)
		return fmt.Errorf("[风控] %s", reason)
	}

	// 4. 连续亏损次数检查
	if c.consecutiveLoss >= c.cfg.MaxConsecutiveLoss {
		reason := fmt.Sprintf("连续亏损 %d 次超过限制 %d 次", c.consecutiveLoss, c.cfg.MaxConsecutiveLoss)
		c.halt(reason)
		return fmt.Errorf("[风控] %s", reason)
	}

	return nil
}

// CheckPnlTarget 检查是否达到止盈/止损目标（由策略层调用）
// 返回 (shouldStop bool, reason string)
func (c *Controller) CheckPnlTarget(takeProfitUSDC, stopLossUSDC float64) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 止盈
	if takeProfitUSDC > 0 && c.totalPnl >= takeProfitUSDC {
		return true, fmt.Sprintf("已达止盈目标 %.4f USDC（当前盈亏 %.4f USDC）", takeProfitUSDC, c.totalPnl)
	}

	// 止损
	if stopLossUSDC > 0 && -c.totalPnl >= stopLossUSDC {
		return true, fmt.Sprintf("已触发止损 %.4f USDC（当前盈亏 %.4f USDC）", stopLossUSDC, c.totalPnl)
	}

	return false, ""
}

// Stats 返回当前风控统计快照（用于日志打印）
func (c *Controller) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		TotalPnl:        c.totalPnl,
		DailyPnl:        c.dailyPnl,
		ConsecutiveLoss: c.consecutiveLoss,
		Halted:          c.halted,
		HaltReason:      c.haltReason,
	}
}

// Reset 手动重置熔断状态（人工干预后调用）
func (c *Controller) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halted = false
	c.haltReason = ""
	c.consecutiveLoss = 0
	log.Println("[风控] 熔断状态已手动重置")
}

// Stats 风控统计快照
type Stats struct {
	TotalPnl        float64
	DailyPnl        float64
	ConsecutiveLoss int
	Halted          bool
	HaltReason      string
}

// ---------- 内部方法 ----------

// maybeResetDaily 如果跨天则重置当日盈亏（调用前需持有锁）
func (c *Controller) maybeResetDaily() {
	now := time.Now()
	nextDay := c.dailyReset.Add(24 * time.Hour)
	if now.After(nextDay) {
		log.Printf("[风控] 新的交易日，重置当日盈亏（昨日: %.4f USDC）", c.dailyPnl)
		c.dailyPnl = 0
		c.dailyReset = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
}

// checkAndHalt 在 RecordTrade 后检查是否需要熔断（调用前需持有锁）
func (c *Controller) checkAndHalt() {
	if c.halted {
		return
	}
	if -c.dailyPnl >= c.cfg.MaxDailyLossUSDC {
		c.halt(fmt.Sprintf("单日亏损 %.4f USDC 超过限制 %.4f USDC", -c.dailyPnl, c.cfg.MaxDailyLossUSDC))
		return
	}
	if c.consecutiveLoss >= c.cfg.MaxConsecutiveLoss {
		c.halt(fmt.Sprintf("连续亏损 %d 次超过限制 %d 次", c.consecutiveLoss, c.cfg.MaxConsecutiveLoss))
	}
}

// halt 触发熔断（调用前需持有锁）
func (c *Controller) halt(reason string) {
	c.halted = true
	c.haltReason = reason
	log.Printf("[风控] ⚠️  熔断触发！原因: %s", reason)
}
