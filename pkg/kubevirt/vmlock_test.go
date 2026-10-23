package kubevirt

import (
	"testing"
	"time"
)

func TestKeyedMutex_SameKeySerializes(t *testing.T) {
	km := newKeyedMutex()

	unlockA := km.Lock("ns/vm")

	acquired := make(chan struct{})
	go func() {
		unlockB := km.Lock("ns/vm")
		close(acquired)
		unlockB()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock on the same key was acquired while the first was still held")
	case <-time.After(50 * time.Millisecond):
	}

	unlockA()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second lock on the same key was not acquired after the first was released")
	}
}

func TestKeyedMutex_DifferentKeysDoNotBlock(t *testing.T) {
	km := newKeyedMutex()

	unlockA := km.Lock("ns/vm-a")
	defer unlockA()

	acquired := make(chan struct{})
	go func() {
		unlockB := km.Lock("ns/vm-b")
		close(acquired)
		unlockB()
	}()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("lock on a different key was blocked by an unrelated holder")
	}
}

func TestKeyedMutex_EntriesAreDroppedWhenUnused(t *testing.T) {
	km := newKeyedMutex()

	unlockA := km.Lock("ns/vm-a")
	unlockB := km.Lock("ns/vm-b")
	if got := km.size(); got != 2 {
		t.Fatalf("expected 2 live entries while both keys are held, got %d", got)
	}

	unlockA()
	if got := km.size(); got != 1 {
		t.Fatalf("expected 1 live entry after releasing one key, got %d", got)
	}

	unlockB()
	if got := km.size(); got != 0 {
		t.Fatalf("expected no live entries after releasing every key, got %d", got)
	}

	// A key that is held and waited for at the same time stays alive until the last waiter is done.
	unlockA = km.Lock("ns/vm-a")
	done := make(chan struct{})
	go func() {
		unlock := km.Lock("ns/vm-a")
		unlock()
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if got := km.size(); got != 1 {
		t.Fatalf("expected the entry to stay alive while a waiter is queued, got %d entries", got)
	}
	unlockA()
	<-done
	if got := km.size(); got != 0 {
		t.Fatalf("expected the entry to be dropped after the waiter finished, got %d entries", got)
	}
}
