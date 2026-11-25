package nfs

import (
	"testing"
	"time"
)

func TestTimeoutSecStored(t *testing.T) {
	d := &Nfs{}
	d.setTimeoutSec(600)
	if d.rpcTimeout != 600*time.Second {
		t.Errorf("rpcTimeout = %v, want 600s", d.rpcTimeout)
	}
	d.setTimeoutSec(0)
	if d.rpcTimeout != 0 {
		t.Errorf("rpcTimeout = %v, want 0 (no timeout)", d.rpcTimeout)
	}
	d.setTimeoutSec(-5)
	if d.rpcTimeout != 0 {
		t.Errorf("rpcTimeout = %v, want 0 (negative clamped)", d.rpcTimeout)
	}
}
