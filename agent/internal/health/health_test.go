package health_test

import (
	"runtime"
	"testing"
	"time"

	"terminalagent/internal/health"
)

func TestCollectHealth(t *testing.T) {
	m1 := health.Collect()
	time.Sleep(100 * time.Millisecond)
	m2 := health.Collect()

	if runtime.GOOS == "windows" {
		if m2.RAMTotalBytes == 0 {
			t.Fatal("expected RAMTotalBytes > 0 on Windows")
		}
		if m2.RAMPercent < 0 || m2.RAMPercent > 100 {
			t.Fatalf("invalid RAM percent: %f", m2.RAMPercent)
		}
		if m2.DiskTotalBytes == 0 {
			t.Fatal("expected DiskTotalBytes > 0 on Windows")
		}
		if m2.UptimeSec <= 0 {
			t.Fatalf("expected UptimeSec > 0 on Windows, got %d", m2.UptimeSec)
		}
	} else {
		if m1.ProcessCount <= 0 {
			t.Fatal("expected positive process count")
		}
	}
}
