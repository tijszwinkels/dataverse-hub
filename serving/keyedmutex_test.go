package serving

import (
	"sync"
	"testing"
	"time"
)

// TestKeyedMutexMutualExclusion: many goroutines incrementing a shared counter
// under the same key must not lose updates (run with -race for full assurance).
func TestKeyedMutexMutualExclusion(t *testing.T) {
	k := newKeyedMutex()
	const goroutines, iters = 50, 200
	counter := 0

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				k.Lock("same")
				counter++
				k.Unlock("same")
			}
		}()
	}
	wg.Wait()

	if want := goroutines * iters; counter != want {
		t.Errorf("counter = %d, want %d (lost updates → not mutually exclusive)", counter, want)
	}
	if n := len(k.locks); n != 0 {
		t.Errorf("idle keys not cleaned up: %d entries remain", n)
	}
}

// TestKeyedMutexDifferentKeysConcurrent: locks on distinct keys must not block
// each other. Two goroutines each hold a different key at the same time; if the
// mutex serialized across keys this would deadlock the rendezvous and time out.
func TestKeyedMutexDifferentKeysConcurrent(t *testing.T) {
	k := newKeyedMutex()
	aHeld := make(chan struct{})
	bHeld := make(chan struct{})
	done := make(chan struct{})

	go func() {
		k.Lock("a")
		close(aHeld)
		<-bHeld // hold "a" until "b" is also held
		k.Unlock("a")
		done <- struct{}{}
	}()
	go func() {
		k.Lock("b")
		close(bHeld)
		<-aHeld // hold "b" until "a" is also held
		k.Unlock("b")
		done <- struct{}{}
	}()

	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("timed out — different keys appear to block each other")
		}
	}
}
