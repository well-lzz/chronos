package solver

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentBumpAndMessageSnapshot(t *testing.T) {
	s := &Solver{}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Bump(fmt.Sprintf("%d/%d", worker, i))
				_ = s.messageSnapshot()
			}
		}(worker)
	}
	wg.Wait()
}
