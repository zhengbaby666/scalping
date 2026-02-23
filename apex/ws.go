package apex

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// WsOrderBook WebSocket 推送的订单簿数据
type WsOrderBook struct {
	Symbol string     `json:"symbol"`
	Bids   [][]string `json:"bids"`
	Asks   [][]string `json:"asks"`
	Ts     int64      `json:"ts"`
}

// WsQuality 连接质量快照
type WsQuality struct {
	Connected     bool          // 当前是否已连接
	RTT           time.Duration // 最近一次 Ping-Pong 往返延迟
	ReconnectCount int          // 累计重连次数
	LastPongAt    time.Time     // 最近一次收到 Pong 的时间
	LastMsgAt     time.Time     // 最近一次收到业务消息的时间
}

// subscription 保存一个订阅的元数据，用于断线后恢复
type subscription struct {
	topic string
	cb    func(data []byte)
}

// WsClient Apex Pro WebSocket 客户端（支持断线重连 + 连接质量监控）
type WsClient struct {
	wsURL string

	mu   sync.Mutex
	conn *websocket.Conn

	// 订阅注册表（断线后自动恢复）
	subsMu sync.RWMutex
	subs   []subscription

	// 连接质量
	connected      atomic.Bool
	reconnectCount atomic.Int64
	lastPongAt     atomic.Value // time.Time
	lastMsgAt      atomic.Value // time.Time
	pingSeq        atomic.Int64
	rtt            atomic.Int64 // nanoseconds

	// 内部控制
	done      chan struct{} // 外部调用 Close() 时关闭
	reconnCh  chan struct{} // readLoop 检测到断线时通知 reconnectLoop
	pingSentAt sync.Map    // seq(string) → time.Time，用于计算 RTT
}

// ---- 重连参数 ----
const (
	wsInitialBackoff = 1 * time.Second
	wsMaxBackoff     = 30 * time.Second
	wsPingInterval   = 20 * time.Second
	wsPongTimeout    = 10 * time.Second // Pong 超时则主动断线触发重连
	wsDialTimeout    = 10 * time.Second
)

// NewWsClient 创建 WebSocket 客户端
func NewWsClient(wsURL string) *WsClient {
	w := &WsClient{
		wsURL:    wsURL,
		done:     make(chan struct{}),
		reconnCh: make(chan struct{}, 1),
	}
	w.lastPongAt.Store(time.Time{})
	w.lastMsgAt.Store(time.Time{})
	return w
}

// Connect 建立初始连接并启动后台 goroutine
func (w *WsClient) Connect() error {
	if err := w.dial(); err != nil {
		return err
	}
	go w.reconnectLoop()
	return nil
}

// SubscribeOrderBook 订阅订单簿频道（断线重连后自动恢复）
func (w *WsClient) SubscribeOrderBook(symbol string, cb func(ob *WsOrderBook)) error {
	topic := fmt.Sprintf("orderbook.%s", symbol)

	// 注册到订阅表
	w.subsMu.Lock()
	w.subs = append(w.subs, subscription{
		topic: topic,
		cb: func(data []byte) {
			var ob WsOrderBook
			if err := json.Unmarshal(data, &ob); err != nil {
				log.Printf("[WS] 解析订单簿数据失败: %v", err)
				return
			}
			cb(&ob)
		},
	})
	w.subsMu.Unlock()

	// 立即发送订阅请求
	return w.sendSubscribe(topic)
}

// Quality 返回当前连接质量快照（线程安全）
func (w *WsClient) Quality() WsQuality {
	lastPong, _ := w.lastPongAt.Load().(time.Time)
	lastMsg, _ := w.lastMsgAt.Load().(time.Time)
	return WsQuality{
		Connected:      w.connected.Load(),
		RTT:            time.Duration(w.rtt.Load()),
		ReconnectCount: int(w.reconnectCount.Load()),
		LastPongAt:     lastPong,
		LastMsgAt:      lastMsg,
	}
}

// IsReady 返回当前是否已连接且可用
func (w *WsClient) IsReady() bool {
	return w.connected.Load()
}

// Close 关闭客户端，停止所有后台 goroutine
func (w *WsClient) Close() {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	w.mu.Lock()
	if w.conn != nil {
		_ = w.conn.Close()
	}
	w.mu.Unlock()
}

// ---- 内部方法 ----

// dial 建立一次 WebSocket 连接，成功后启动 readLoop / pingLoop
func (w *WsClient) dial() error {
	dialer := websocket.Dialer{HandshakeTimeout: wsDialTimeout}
	conn, _, err := dialer.Dial(w.wsURL, nil)
	if err != nil {
		return fmt.Errorf("[WS] 连接失败: %w", err)
	}

	// 设置 Pong 处理器：记录时间 + 计算 RTT
	conn.SetPongHandler(func(appData string) error {
		now := time.Now()
		w.lastPongAt.Store(now)
		// 用 appData 存放 seq，计算 RTT
		if sentVal, ok := w.pingSentAt.LoadAndDelete(appData); ok {
			if sentTime, ok2 := sentVal.(time.Time); ok2 {
				rtt := now.Sub(sentTime)
				w.rtt.Store(int64(rtt))
				log.Printf("[WS] Pong 收到，RTT=%.2fms", float64(rtt.Microseconds())/1000.0)
			}
		}
		return nil
	})

	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()

	w.connected.Store(true)
	log.Printf("[WS] 连接成功: %s", w.wsURL)

	go w.readLoop(conn)
	go w.pingLoop(conn)
	return nil
}

// reconnectLoop 监听断线信号，执行指数退避重连
func (w *WsClient) reconnectLoop() {
	backoff := wsInitialBackoff
	for {
		select {
		case <-w.done:
			return
		case <-w.reconnCh:
			w.connected.Store(false)
			count := w.reconnectCount.Add(1)
			log.Printf("[WS] 检测到断线，第 %d 次重连，等待 %v ...", count, backoff)

			select {
			case <-w.done:
				return
			case <-time.After(backoff):
			}

			if err := w.dial(); err != nil {
				log.Printf("[WS] 重连失败: %v", err)
				// 指数退避，上限 30s
				backoff *= 2
				if backoff > wsMaxBackoff {
					backoff = wsMaxBackoff
				}
				// 再次触发重连
				select {
				case w.reconnCh <- struct{}{}:
				default:
				}
				continue
			}

			// 重连成功，重置退避
			backoff = wsInitialBackoff
			log.Printf("[WS] 重连成功，恢复 %d 个订阅...", w.subCount())
			w.resubscribeAll()
		}
	}
}

// readLoop 持续读取消息，断线时通知 reconnectLoop
func (w *WsClient) readLoop(conn *websocket.Conn) {
	defer func() {
		// 通知重连（仅当不是主动关闭时）
		select {
		case <-w.done:
			return
		default:
			select {
			case w.reconnCh <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-w.done:
			return
		default:
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-w.done:
			default:
				log.Printf("[WS] 读取错误（将触发重连）: %v", err)
			}
			return
		}

		w.lastMsgAt.Store(time.Now())

		var envelope struct {
			Topic string          `json:"topic"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}
		if envelope.Topic == "" {
			continue
		}

		w.subsMu.RLock()
		for _, s := range w.subs {
			if s.topic == envelope.Topic {
				s.cb(envelope.Data)
				break
			}
		}
		w.subsMu.RUnlock()
	}
}

// pingLoop 定时发送 Ping，并检测 Pong 超时（超时则主动断线触发重连）
func (w *WsClient) pingLoop(conn *websocket.Conn) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			// 检查上次 Pong 是否超时
			if lastPong, ok := w.lastPongAt.Load().(time.Time); ok && !lastPong.IsZero() {
				if time.Since(lastPong) > wsPingInterval+wsPongTimeout {
					log.Printf("[WS] Pong 超时（超过 %v 未收到），主动断线触发重连", wsPingInterval+wsPongTimeout)
					_ = conn.Close()
					return
				}
			}

			// 发送带序号的 Ping（序号写入 appData，用于 RTT 计算）
			seq := fmt.Sprintf("%d", w.pingSeq.Add(1))
			w.pingSentAt.Store(seq, time.Now())

			w.mu.Lock()
			err := conn.WriteMessage(websocket.PingMessage, []byte(seq))
			w.mu.Unlock()

			if err != nil {
				log.Printf("[WS] Ping 发送失败: %v", err)
				return
			}
		}
	}
}

// resubscribeAll 重连后重新发送所有订阅请求
func (w *WsClient) resubscribeAll() {
	w.subsMu.RLock()
	defer w.subsMu.RUnlock()
	for _, s := range w.subs {
		if err := w.sendSubscribe(s.topic); err != nil {
			log.Printf("[WS] 恢复订阅 %s 失败: %v", s.topic, err)
		} else {
			log.Printf("[WS] 已恢复订阅: %s", s.topic)
		}
	}
}

// sendSubscribe 发送订阅报文
func (w *WsClient) sendSubscribe(topic string) error {
	msg := map[string]interface{}{
		"op":   "subscribe",
		"args": []string{topic},
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("连接尚未建立")
	}
	return w.conn.WriteJSON(msg)
}

// subCount 返回当前订阅数量
func (w *WsClient) subCount() int {
	w.subsMu.RLock()
	defer w.subsMu.RUnlock()
	return len(w.subs)
}