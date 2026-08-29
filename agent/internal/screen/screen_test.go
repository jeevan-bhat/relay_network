package screen

import (
	"testing"
)

func TestCaptureScreen(t *testing.T) {
	resp, err := CaptureScreen("test_dev", 70, 1280)
	if err != nil {
		t.Logf("Screen capture error (headless environment expected): %v", err)
		return
	}
	if resp.ImageBase64 == "" {
		t.Errorf("expected non-empty imageBase64")
	}
	if resp.Width <= 0 || resp.Height <= 0 {
		t.Errorf("expected positive dimensions, got %dx%d", resp.Width, resp.Height)
	}
}
