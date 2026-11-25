package webdav

import (
	"testing"
	"time"
)

func TestSetTimeoutSec(t *testing.T) {
	d := NewWebdavDrive("https://example.com", "user", "pass", false)
	// 构造默认 60s
	if d.cliTimeout() != 60*time.Second {
		t.Errorf("default timeout = %v, want 60s", d.cliTimeout())
	}
	d.SetTimeoutSec(600)
	if d.cliTimeout() != 600*time.Second {
		t.Errorf("timeout after SetTimeoutSec(600) = %v, want 600s", d.cliTimeout())
	}
	d.SetTimeoutSec(0)
	if d.cliTimeout() != 0 {
		t.Errorf("timeout after SetTimeoutSec(0) = %v, want 0 (no timeout)", d.cliTimeout())
	}
	d.SetTimeoutSec(-5)
	if d.cliTimeout() != 0 {
		t.Errorf("timeout after SetTimeoutSec(-5) = %v, want 0 (negative treated as no timeout)", d.cliTimeout())
	}
}
