package main

import (
	"log"
	"time"
)

type ProactiveActionWorker struct {
	cognition *CognitionClient
	engine    *HostEngine
	interval  time.Duration
	stopCh    chan struct{}
}

func NewProactiveActionWorker(cognition *CognitionClient, engine *HostEngine, interval time.Duration) *ProactiveActionWorker {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &ProactiveActionWorker{
		cognition: cognition,
		engine:    engine,
		interval:  interval,
		stopCh:    make(chan struct{}),
	}
}

func (w *ProactiveActionWorker) Start() {
	go func() {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick()
			case <-w.stopCh:
				return
			}
		}
	}()
}

func (w *ProactiveActionWorker) Stop() {
	close(w.stopCh)
}

func (w *ProactiveActionWorker) tick() {
	action, err := w.cognition.ClaimAction()
	if err != nil {
		log.Printf("[Autonomy] 领取主动动作失败: %v", err)
		return
	}
	if action == nil {
		return
	}

	output, _, execErr := w.engine.Execute(action.Command, action.Context)
	result := &ActionResultRequest{
		Output:  output,
		Success: execErr == nil,
	}
	if execErr != nil {
		result.ErrorText = execErr.Error()
		log.Printf("[Autonomy] 执行动作失败 (%s): %v", action.ID, execErr)
	}

	if err := w.cognition.SubmitActionResult(action.ID, result); err != nil {
		log.Printf("[Autonomy] 回写动作结果失败 (%s): %v", action.ID, err)
	}
}
