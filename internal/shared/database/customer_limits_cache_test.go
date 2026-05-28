package database

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/models"
)

func TestCustomerLimitCache_MissOnEmpty(t *testing.T) {
	c := newCustomerLimitCache(time.Minute)
	_, ok := c.get(uuid.New(), "cust-1")
	if ok {
		t.Error("expected miss on empty cache, got hit")
	}
}

func TestCustomerLimitCache_SetThenGet(t *testing.T) {
	c := newCustomerLimitCache(time.Minute)
	pid := uuid.New()
	want := &models.CustomerLimit{CustomerID: "cust-1"}

	c.set(pid, "cust-1", want)
	got, ok := c.get(pid, "cust-1")
	if !ok {
		t.Fatal("expected hit, got miss")
	}
	if got != want {
		t.Errorf("returned wrong entry: got %p, want %p", got, want)
	}
}

func TestCustomerLimitCache_NegativeCaching(t *testing.T) {
	c := newCustomerLimitCache(time.Minute)
	pid := uuid.New()

	c.set(pid, "cust-1", nil)
	got, ok := c.get(pid, "cust-1")
	if !ok {
		t.Fatal("negative cache should register as hit, got miss")
	}
	if got != nil {
		t.Errorf("negative cache should return nil, got %v", got)
	}
}

func TestCustomerLimitCache_Expiry(t *testing.T) {
	c := newCustomerLimitCache(10 * time.Millisecond)
	pid := uuid.New()
	c.set(pid, "cust-1", &models.CustomerLimit{})

	time.Sleep(20 * time.Millisecond)
	_, ok := c.get(pid, "cust-1")
	if ok {
		t.Error("expected expired entry to miss, got hit")
	}
}

func TestCustomerLimitCache_Invalidate(t *testing.T) {
	c := newCustomerLimitCache(time.Minute)
	pid := uuid.New()
	c.set(pid, "cust-1", &models.CustomerLimit{})

	c.invalidate(pid, "cust-1")
	_, ok := c.get(pid, "cust-1")
	if ok {
		t.Error("expected miss after invalidate, got hit")
	}
}

func TestCustomerLimitCache_DifferentProjects_Isolated(t *testing.T) {
	c := newCustomerLimitCache(time.Minute)
	p1 := uuid.New()
	p2 := uuid.New()

	c.set(p1, "cust-1", &models.CustomerLimit{CustomerID: "p1-cust"})
	c.set(p2, "cust-1", &models.CustomerLimit{CustomerID: "p2-cust"})

	v1, _ := c.get(p1, "cust-1")
	v2, _ := c.get(p2, "cust-1")
	if v1 == v2 {
		t.Error("same customer ID in different projects should cache independently")
	}
	if v1.CustomerID != "p1-cust" || v2.CustomerID != "p2-cust" {
		t.Errorf("crossed cache entries: v1=%s v2=%s", v1.CustomerID, v2.CustomerID)
	}
}

func TestCustomerLimitCache_ConcurrentSafe(t *testing.T) {
	c := newCustomerLimitCache(time.Minute)
	pid := uuid.New()

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			c.set(pid, "cust", &models.CustomerLimit{})
			_, _ = c.get(pid, "cust")
			c.invalidate(pid, "cust")
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}
