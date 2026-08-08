package track

import (
	"math"
	"sync"
	"testing"
)

type sequenceEntropy struct {
	mu   sync.Mutex
	next uint64
}

func (s *sequenceEntropy) Read(buffer []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range buffer {
		buffer[index] = byte(s.next)
		s.next++
	}
	return len(buffer), nil
}

func (s *sequenceEntropy) Uint64n(limit uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.next % limit
	s.next++
	return value, nil
}

func TestLoadDefaultInvariants(t *testing.T) {
	events, err := LoadDefault(80, &sequenceEntropy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < minimumEvents || len(events) > 86 {
		t.Fatalf("unexpected event count %d", len(events))
	}
	if events[0].Type != "touchstart" || events[0].X != 0 || events[0].DT != 0 {
		t.Fatalf("invalid first event: %+v", events[0])
	}
	last := events[len(events)-1]
	if last.Type != "touchend" || last.X != 80 {
		t.Fatalf("invalid last event: %+v", last)
	}
	if events[len(events)-2].Type != "touchmove" || events[len(events)-2].X != 80 {
		t.Fatalf("last move must pin target: %+v", events[len(events)-2])
	}
	for index, event := range events {
		if event.DT < 0 || event.Force < 0 || event.Force > 1 || event.RadiusX <= 0 || event.RadiusY <= 0 {
			t.Fatalf("invalid event %d: %+v", index, event)
		}
	}
}

func TestLoadDefaultRejectsDistance(t *testing.T) {
	for _, value := range []float64{-1, math.Inf(1)} {
		if _, err := LoadDefault(value, &sequenceEntropy{}); err == nil {
			t.Fatalf("expected error for %v", value)
		}
	}
}

func TestLoadDefaultConcurrent(t *testing.T) {
	const workers = 32
	var group sync.WaitGroup
	group.Add(workers)
	errors := make(chan error, workers)
	for index := 0; index < workers; index++ {
		go func(seed uint64) {
			defer group.Done()
			_, err := LoadDefault(120, &sequenceEntropy{next: seed})
			errors <- err
		}(uint64(index))
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
}
