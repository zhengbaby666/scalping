package strategy

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"apex-scalping/apex"
	"apex-scalping/config"
	"apex-scalping/risk"
)

// ScalpingEngine 剥皮头策略引擎
type ScalpingEngine struct {
	cfg    *config.Config
	client *apex.Client
	ws     *apex.WsClient
	rc     *risk.Controller

	// 当前最优买一/卖一价（原子读写，避免锁竞争）
	bestBid atomic.Value // float64
	bestAsk atomic.Value // float64

	// 当前挂单 ID
	mu          sync.Mutex
	buyOrderID  string
	sellOrderID string

	// 持仓快照（用于估算已实现盈亏）
	lastPosition   float64
	lastEntryPrice float64

	// ---- 趋势过滤 + 动态 Spread ----
	candleAgg    *CandleAggregator // K 线聚合器
	trendFilter  *TrendFilter      // 双 EMA 趋势过滤器
	atr          *ATR              // ATR 计算器
	currentTrend TrendDir          // 当前趋势方向（原子读写用 int32）
	dynSpreadTks atomic.Value      // 当前动态 spread ticks（int）

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewScalpingEngine 创建策略引擎
func NewScalpingEngine(cfg *config.Config) (*ScalpingEngine, error) {
	client := apex.NewClient(
		cfg.API.BaseURL,
		cfg.API.APIKey,
		cfg.API.APISecret,
		cfg.API.Passphrase,
	)
	ws := apex.NewWsClient(cfg.API.WsURL)
	rc := risk.NewController(cfg.RiskControl)

	// 初始化 K 线聚合器（默认 60s，若配置为 0 则使用默认值）
	candlePeriod := cfg.Strategy.TrendFilter.CandlePeriodSec
	if candlePeriod <= 0 {
		candlePeriod = 60
	}

	// 初始化趋势过滤器
	fastPeriod := cfg.Strategy.TrendFilter.FastEMAPeriod
	slowPeriod := cfg.Strategy.TrendFilter.SlowEMAPeriod
	threshold := cfg.Strategy.TrendFilter.Threshold
	if fastPeriod <= 0 {
		fastPeriod = 9
	}
	if slowPeriod <= 0 {
		slowPeriod = 21
	}
	if threshold <= 0 {
		threshold = 0.0003
	}

	// 初始化 ATR
	atrPeriod := cfg.Strategy.DynamicSpread.ATRPeriod
	if atrPeriod <= 0 {
		atrPeriod = 14
	}

	e := &ScalpingEngine{
		cfg:         cfg,
		client:      client,
		ws:          ws,
		rc:          rc,
		candleAgg:   NewCandleAggregator(candlePeriod),
		trendFilter: NewTrendFilter(fastPeriod, slowPeriod, threshold),
		atr:         NewATR(atrPeriod),
		stopCh:      make(chan struct{}),
	}
	// 初始化动态 spread 为配置的静态值
	e.dynSpreadTks.Store(cfg.Strategy.SpreadTicks)
	return e, nil
}

// Start 启动策略
func (e *ScalpingEngine) Start() error {
	// 1. 建立 WebSocket 连接
	if err := e.ws.Connect(); err != nil {
		return fmt.Errorf("WebSocket 连接失败: %w", err)
	}

	// 2. 等待连接就绪（最多 10 秒）
	log.Println("[策略] 等待 WebSocket 连接就绪...")
	deadline := time.Now().Add(10 * time.Second)
	for !e.ws.IsReady() {
		if time.Now().After(deadline) {
			return fmt.Errorf("WebSocket 连接超时（10s）")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3. 订阅订单簿
	if err := e.ws.SubscribeOrderBook(e.cfg.Symbol, e.onOrderBook); err != nil {
		return fmt.Errorf("订阅订单簿失败: %w", err)
	}

	// 4. 启动后台 goroutine
	e.wg.Add(4)
	go e.mainLoop()         // 主循环：500ms 刷新挂单
	go e.pnlMonitorLoop()   // 盈亏监控：2s 检查持仓变化
	go e.wsQualityMonitor() // 连接质量：30s 打印报告
	go e.indicatorLoop()    // 指标更新：K 线聚合 → ATR/EMA 更新

	log.Printf("[策略] 已启动，交易对=%s 价差=%d ticks 单笔=%v 趋势过滤=%v 动态Spread=%v",
		e.cfg.Symbol, e.cfg.Strategy.SpreadTicks, e.cfg.Strategy.OrderSize,
		e.cfg.Strategy.TrendFilter.Enabled, e.cfg.Strategy.DynamicSpread.Enabled)
	return nil
}

// Stop 停止策略（优雅退出）
func (e *ScalpingEngine) Stop() {
	close(e.stopCh)
	e.wg.Wait()
	e.ws.Close()

	log.Println("[策略] 撤销所有挂单...")
	if err := e.client.CancelAllOrders(e.cfg.Symbol); err != nil {
		log.Printf("[策略] 撤单失败: %v", err)
	}

	stats := e.rc.Stats()
	q := e.ws.Quality()
	log.Printf("[策略] 已停止 | 累计盈亏=%.4f USDC 当日盈亏=%.4f USDC | WS重连次数=%d",
		stats.TotalPnl, stats.DailyPnl, q.ReconnectCount)
}

// ---------- 订单簿回调 ----------

// onOrderBook WebSocket 订单簿推送回调，更新最优买一/卖一价
func (e *ScalpingEngine) onOrderBook(ob *apex.WsOrderBook) {
	if len(ob.Bids) > 0 {
		if p, err := strconv.ParseFloat(ob.Bids[0][0], 64); err == nil {
			e.bestBid.Store(p)
		}
	}
	if len(ob.Asks) > 0 {
		if p, err := strconv.ParseFloat(ob.Asks[0][0], 64); err == nil {
			e.bestAsk.Store(p)
		}
	}
}

// ---------- 主循环 ----------

// mainLoop 每 RefreshIntervalMs 执行一次 tick
func (e *ScalpingEngine) mainLoop() {
	defer e.wg.Done()

	interval := time.Duration(e.cfg.Strategy.RefreshIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			if err := e.tick(); err != nil {
				log.Printf("[策略] tick 错误: %v", err)
			}
		}
	}
}

// tick 单次执行：风控检查 → WS 状态检查 → 趋势过滤 → 动态 Spread → 计算报价 → 刷新挂单
func (e *ScalpingEngine) tick() error {
	// ---- Step 1: 风控检查 ----
	account, err := e.client.GetAccount()
	if err != nil {
		return fmt.Errorf("获取账户失败: %w", err)
	}
	if err := e.rc.Check(account.AvailableValue); err != nil {
		log.Printf("[风控] 拦截下单: %v", err)
		e.cancelBothOrders()
		return nil
	}

	// ---- Step 2: WebSocket 连接状态检查 ----
	if !e.ws.IsReady() {
		log.Printf("[策略] WebSocket 断线中，暂停下单，等待重连...")
		e.cancelBothOrders()
		return nil
	}

	// ---- Step 3: 获取最优价 ----
	bid, ask, ok := e.getBestPrices()
	if !ok {
		return nil // 尚未收到行情
	}

	spread := ask - bid
	if spread < e.cfg.Strategy.MinSpreadUSDC {
		log.Printf("[策略] 价差 %.4f < 最小价差 %.4f，跳过本轮", spread, e.cfg.Strategy.MinSpreadUSDC)
		return nil
	}

	// ---- Step 4: 动态 Spread 计算 ----
	spreadTicks := e.getSpreadTicks()

	// ---- Step 5: 计算挂单价格 ----
	offset := float64(spreadTicks) * e.tickSize()
	buyPrice := e.roundPrice(bid + offset)
	sellPrice := e.roundPrice(ask - offset)

	if buyPrice >= sellPrice {
		log.Printf("[策略] 计算价格异常 buy=%.4f sell=%.4f，跳过", buyPrice, sellPrice)
		return nil
	}

	// ---- Step 6: 查询持仓 ----
	pos, err := e.getPosition()
	if err != nil {
		return fmt.Errorf("获取持仓失败: %w", err)
	}

	sizeStr := fmt.Sprintf("%.*f", e.cfg.Strategy.SizePrecision, e.cfg.Strategy.OrderSize)

	// ---- Step 7: 趋势过滤 → 决定挂单方向 ----
	allowBuy, allowSell := e.getAllowedSides(pos)

	// ---- Step 8: 撤旧单 → 挂新单 ----
	e.cancelBothOrders()

	maxPos := e.cfg.Strategy.MaxPosition
	if allowBuy && math.Abs(pos) < maxPos {
		if err := e.placeBuyOrder(buyPrice, sizeStr); err != nil {
			log.Printf("[策略] 挂买单失败: %v", err)
		}
	}
	if allowSell && math.Abs(pos) < maxPos {
		if err := e.placeSellOrder(sellPrice, sizeStr); err != nil {
			log.Printf("[策略] 挂卖单失败: %v", err)
		}
	}

	log.Printf("[策略] 刷新挂单 bid=%.4f ask=%.4f buy=%.4f sell=%.4f pos=%.4f spread=%.4f spreadTicks=%d 趋势=%s",
		bid, ask, buyPrice, sellPrice, pos, spread, spreadTicks, e.currentTrend)
	return nil
}

// getSpreadTicks 获取当前有效的 spread ticks（动态或静态）
func (e *ScalpingEngine) getSpreadTicks() int {
	if !e.cfg.Strategy.DynamicSpread.Enabled {
		return e.cfg.Strategy.SpreadTicks
	}
	if v := e.dynSpreadTks.Load(); v != nil {
		return v.(int)
	}
	return e.cfg.Strategy.SpreadTicks
}

// getAllowedSides 根据趋势方向决定允许挂单的方向
// 返回 (allowBuy, allowSell)
func (e *ScalpingEngine) getAllowedSides(pos float64) (bool, bool) {
	if !e.cfg.Strategy.TrendFilter.Enabled {
		return true, true
	}

	// 指标尚未就绪，双边挂单（预热阶段）
	if !e.trendFilter.Ready() {
		return true, true
	}

	switch e.currentTrend {
	case TrendUp:
		// 上升趋势：只允许买单（做多方向）
		// 若已有多头持仓超过阈值，也允许挂卖单平仓
		allowSell := pos > 0 // 有多头持仓时允许挂卖单平仓
		return true, allowSell
	case TrendDown:
		// 下降趋势：只允许卖单（做空方向）
		// 若已有空头持仓超过阈值，也允许挂买单平仓
		allowBuy := pos < 0 // 有空头持仓时允许挂买单平仓
		return allowBuy, true
	default:
		// 横盘：双边挂单
		return true, true
	}
}

// ---------- 指标更新循环 ----------

// indicatorLoop 每 100ms 检查一次是否有新 K 线完成，更新 ATR/EMA/趋势
func (e *ScalpingEngine) indicatorLoop() {
	defer e.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.updateIndicators()
		}
	}
}

// updateIndicators 用当前中间价 feed K 线聚合器，若有新 K 线则更新 ATR/EMA
func (e *ScalpingEngine) updateIndicators() {
	bid, ask, ok := e.getBestPrices()
	if !ok {
		return
	}
	mid := (bid + ask) / 2
	nowSec := time.Now().Unix()

	closed := e.candleAgg.Feed(mid, nowSec)
	if closed == nil {
		return // 当前 K 线尚未完成
	}

	// 更新 ATR
	atrVal := e.atr.Update(closed.High, closed.Low, closed.Close)

	// 更新趋势过滤器
	trend := e.trendFilter.Update(closed.Close)
	e.currentTrend = trend

	// 更新动态 spread ticks
	if e.cfg.Strategy.DynamicSpread.Enabled && e.atr.Ready() {
		ds := e.cfg.Strategy.DynamicSpread
		tickSz := e.tickSize()
		if tickSz > 0 {
			rawTicks := int(math.Round(atrVal * ds.Multiplier / tickSz))
			// clamp 到 [min_ticks, max_ticks]
			if rawTicks < ds.MinTicks {
				rawTicks = ds.MinTicks
			}
			if ds.MaxTicks > 0 && rawTicks > ds.MaxTicks {
				rawTicks = ds.MaxTicks
			}
			e.dynSpreadTks.Store(rawTicks)
			log.Printf("[指标] K线完成 close=%.4f ATR=%.4f spreadTicks=%d 趋势=%s EMA快=%.4f EMA慢=%.4f",
				closed.Close, atrVal, rawTicks, trend,
				e.trendFilter.FastValue(), e.trendFilter.SlowValue())
		}
	} else {
		log.Printf("[指标] K线完成 close=%.4f ATR=%.4f(预热中) 趋势=%s EMA快=%.4f EMA慢=%.4f",
			closed.Close, atrVal, trend,
			e.trendFilter.FastValue(), e.trendFilter.SlowValue())
	}
}

// ---------- 盈亏监控 ----------

// pnlMonitorLoop 每 2s 检查持仓变化，估算已实现盈亏，触发止盈止损
func (e *ScalpingEngine) pnlMonitorLoop() {
	defer e.wg.Done()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.checkPnlAndPosition()
		}
	}
}

func (e *ScalpingEngine) checkPnlAndPosition() {
	positions, err := e.client.GetPositions()
	if err != nil {
		log.Printf("[风控] 获取持仓失败: %v", err)
		return
	}

	var curPos, curEntryPrice float64
	for _, p := range positions {
		if p.Symbol == e.cfg.Symbol {
			if p.Side == "LONG" {
				curPos = p.Size
			} else {
				curPos = -p.Size
			}
			curEntryPrice = p.EntryPrice
			break
		}
	}

	// 检测持仓减少 → 估算已实现盈亏
	prevPos := e.lastPosition
	if prevPos != 0 && math.Abs(curPos) < math.Abs(prevPos) {
		closedSize := math.Abs(prevPos) - math.Abs(curPos)
		var pnl float64
		if prevPos > 0 {
			if bid, _, ok := e.getBestPrices(); ok {
				pnl = (bid - e.lastEntryPrice) * closedSize
			}
		} else {
			if _, ask, ok := e.getBestPrices(); ok {
				pnl = (e.lastEntryPrice - ask) * closedSize
			}
		}
		if pnl != 0 {
			e.rc.RecordTrade(risk.TradeResult{Pnl: pnl})
			stats := e.rc.Stats()
			log.Printf("[盈亏] 成交平仓 size=%.4f pnl=%.4f USDC | 累计=%.4f 当日=%.4f 连续亏损=%d",
				closedSize, pnl, stats.TotalPnl, stats.DailyPnl, stats.ConsecutiveLoss)
		}
	}

	e.lastPosition = curPos
	e.lastEntryPrice = curEntryPrice

	// 止盈止损检查
	if shouldStop, reason := e.rc.CheckPnlTarget(e.cfg.Strategy.TakeProfitUSDC, e.cfg.Strategy.StopLossUSDC); shouldStop {
		log.Printf("[风控] 触发止盈/止损，策略即将停止: %s", reason)
		e.cancelBothOrders()
		select {
		case <-e.stopCh:
		default:
			close(e.stopCh)
		}
	}

	// 定期统计（含 WS 连接质量）
	stats := e.rc.Stats()
	q := e.ws.Quality()
	log.Printf("[统计] 持仓=%.4f 均价=%.4f | 累计盈亏=%.4f 当日盈亏=%.4f USDC | WS RTT=%v 重连=%d",
		curPos, curEntryPrice, stats.TotalPnl, stats.DailyPnl,
		q.RTT.Round(time.Millisecond), q.ReconnectCount)
}

// ---------- 连接质量监控 ----------

// wsQualityMonitor 每 30s 打印一次 WebSocket 连接质量报告
func (e *ScalpingEngine) wsQualityMonitor() {
	defer e.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			q := e.ws.Quality()
			if q.Connected {
				log.Printf("[WS质量] ✅ 已连接 RTT=%.2fms 重连次数=%d 最近消息=%s 最近Pong=%s",
					float64(q.RTT.Microseconds())/1000.0,
					q.ReconnectCount,
					formatAgo(q.LastMsgAt),
					formatAgo(q.LastPongAt))
			} else {
				log.Printf("[WS质量] ⚠️  未连接，重连次数=%d，最近消息=%s",
					q.ReconnectCount,
					formatAgo(q.LastMsgAt))
			}
		}
	}
}

// ---------- 辅助方法 ----------

func (e *ScalpingEngine) getBestPrices() (bid, ask float64, ok bool) {
	bidVal := e.bestBid.Load()
	askVal := e.bestAsk.Load()
	if bidVal == nil || askVal == nil {
		return 0, 0, false
	}
	return bidVal.(float64), askVal.(float64), true
}

func (e *ScalpingEngine) getPosition() (float64, error) {
	positions, err := e.client.GetPositions()
	if err != nil {
		return 0, err
	}
	for _, p := range positions {
		if p.Symbol == e.cfg.Symbol {
			if p.Side == "LONG" {
				return p.Size, nil
			}
			return -p.Size, nil
		}
	}
	return 0, nil
}

func (e *ScalpingEngine) placeBuyOrder(price float64, size string) error {
	priceStr := fmt.Sprintf("%.*f", e.cfg.Strategy.PricePrecision, price)
	req := &apex.PlaceOrderReq{
		Symbol:      e.cfg.Symbol,
		Side:        "BUY",
		Type:        "LIMIT",
		Size:        size,
		Price:       priceStr,
		TimeInForce: "POST_ONLY",
		ReduceOnly:  false,
	}
	order, err := e.client.PlaceOrder(req)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.buyOrderID = order.ID
	e.mu.Unlock()
	log.Printf("[下单] 买单 id=%s price=%s size=%s", order.ID, priceStr, size)
	return nil
}

func (e *ScalpingEngine) placeSellOrder(price float64, size string) error {
	priceStr := fmt.Sprintf("%.*f", e.cfg.Strategy.PricePrecision, price)
	req := &apex.PlaceOrderReq{
		Symbol:      e.cfg.Symbol,
		Side:        "SELL",
		Type:        "LIMIT",
		Size:        size,
		Price:       priceStr,
		TimeInForce: "POST_ONLY",
		ReduceOnly:  false,
	}
	order, err := e.client.PlaceOrder(req)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.sellOrderID = order.ID
	e.mu.Unlock()
	log.Printf("[下单] 卖单 id=%s price=%s size=%s", order.ID, priceStr, size)
	return nil
}

func (e *ScalpingEngine) cancelBothOrders() {
	e.mu.Lock()
	buyID := e.buyOrderID
	sellID := e.sellOrderID
	e.buyOrderID = ""
	e.sellOrderID = ""
	e.mu.Unlock()

	if buyID != "" {
		if err := e.client.CancelOrder(buyID); err != nil {
			log.Printf("[撤单] 买单 %s 撤销失败: %v", buyID, err)
		} else {
			log.Printf("[撤单] 买单 %s 已撤销", buyID)
		}
	}
	if sellID != "" {
		if err := e.client.CancelOrder(sellID); err != nil {
			log.Printf("[撤单] 卖单 %s 撤销失败: %v", sellID, err)
		} else {
			log.Printf("[撤单] 卖单 %s 已撤销", sellID)
		}
	}
}

func (e *ScalpingEngine) tickSize() float64 {
	return math.Pow(10, -float64(e.cfg.Strategy.PricePrecision))
}

func (e *ScalpingEngine) roundPrice(price float64) float64 {
	factor := math.Pow(10, float64(e.cfg.Strategy.PricePrecision))
	return math.Round(price*factor) / factor
}

// formatAgo 将时间格式化为 "X秒前" / "X分钟前" / "从未"
func formatAgo(t time.Time) string {
	if t.IsZero() {
		return "从未"
	}
	d := time.Since(t)
	if d < time.Minute {
		return fmt.Sprintf("%.0f秒前", d.Seconds())
	}
	return fmt.Sprintf("%.0f分钟前", d.Minutes())
}
