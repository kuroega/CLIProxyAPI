package cursor

import "testing"

func TestBlobStorePoolReusesConversationStore(t *testing.T) {
	pool := NewBlobStorePool(2)
	first := pool.ForSession("conversation-a")
	if errSet := first.Set([]byte("blob"), []byte("value")); errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}
	got := pool.ForSession("conversation-a").Get([]byte("blob"))
	if string(got) != "value" {
		t.Fatalf("Get() = %q, want value", got)
	}
	got[0] = 'x'
	if string(pool.ForSession("conversation-a").Get([]byte("blob"))) != "value" {
		t.Fatal("Get() exposed the stored blob backing array")
	}
}

func TestBlobStorePoolEvictsLeastRecentlyUsedSession(t *testing.T) {
	pool := NewBlobStorePool(2)
	first := pool.ForSession("first")
	if errSet := first.Set([]byte("blob"), []byte("first")); errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}
	_ = pool.ForSession("second")
	_ = pool.ForSession("third")
	if got := pool.ForSession("first").Get([]byte("blob")); got != nil {
		t.Fatalf("evicted session retained blob %q", got)
	}
}

func TestBlobStoreLimitsStoredData(t *testing.T) {
	store := NewBlobStorePool(1).ForSession("conversation")
	if errSet := store.Set([]byte("large"), make([]byte, maxBlobSize+1)); errSet == nil {
		t.Fatal("Set() accepted an oversized blob")
	}
}
