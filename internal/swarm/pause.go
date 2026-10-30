package swarm

import "context"

// Pause requests a cooperative stop at the next safe boundary. An in-flight
// model request or tool call may finish; no subsequent call starts until Resume.
func (s *Swarm) Pause() {
	s.pauseMu.Lock()
	if s.paused.Load() {
		s.pauseMu.Unlock()
		return
	}
	s.paused.Store(true)
	s.resumeCh = make(chan struct{})
	s.mu.Lock()
	if s.status == nil {
		s.status = map[string]string{}
	}
	for _, spec := range s.ModelSpecs {
		if !terminalStatus(s.status[spec]) && s.status[spec] != "paused" {
			s.status[spec] = "pausing"
		}
	}
	s.mu.Unlock()
	s.pauseMu.Unlock()
	for _, spec := range s.ModelSpecs {
		s.emit(spec, 0, "status", "pause", "暂停请求已接收；当前请求或工具完成后暂停。")
	}
}

func (s *Swarm) Resume() {
	s.pauseMu.Lock()
	if !s.paused.Load() {
		s.pauseMu.Unlock()
		return
	}
	s.paused.Store(false)
	if s.resumeCh != nil {
		close(s.resumeCh)
		s.resumeCh = nil
	}
	s.mu.Lock()
	for _, spec := range s.ModelSpecs {
		if s.status[spec] == "paused" || s.status[spec] == "pausing" {
			s.status[spec] = "running"
		}
	}
	s.mu.Unlock()
	s.pauseMu.Unlock()
	for _, spec := range s.ModelSpecs {
		s.emit(spec, 0, "status", "resume", "执行已恢复。")
	}
}

func terminalStatus(status string) bool {
	return status == "finished" || status == "cancelled" || status == "flag_found"
}

// waitIfPaused is shared by the parent loop, model/tool loop and cold digest.
// The channel makes Resume prompt and context cancellation interrupt the wait.
func (s *Swarm) waitIfPaused(ctx context.Context, spec string) error {
	var acknowledged chan struct{}
	for {
		s.pauseMu.Lock()
		paused, resume := s.paused.Load(), s.resumeCh
		if !paused {
			if acknowledged != nil && spec != "" {
				s.setStatus(spec, "running")
			}
			s.pauseMu.Unlock()
			return ctx.Err()
		}
		if resume != acknowledged {
			acknowledged = resume
			if spec != "" {
				s.setStatus(spec, "paused")
				s.emit(spec, 0, "status", "pause", "执行已暂停，等待操作员继续。")
			}
		}
		s.pauseMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resume:
		}
	}
}
