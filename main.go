package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"apex-scalping/config"
	"apex-scalping/strategy"
)

func main() {
	// 加载配置
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	log.Printf("启动 Apex 剥皮头策略，交易对: %s", cfg.Symbol)

	// 初始化策略引擎
	engine, err := strategy.NewScalpingEngine(cfg)
	if err != nil {
		log.Fatalf("初始化策略引擎失败: %v", err)
	}

	// 启动策略
	if err := engine.Start(); err != nil {
		log.Fatalf("启动策略失败: %v", err)
	}

	// 等待退出信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("收到退出信号，正在停止策略...")
	engine.Stop()
	log.Println("策略已安全停止")
}
