package smb

import (
	"io"
	"net"
	"testing"
	"time"
)

// deadlineReader 每次 Read 前把 conn 的读写 deadline 续到 now+timeout。
// 行为验证：inner reader 就是 conn 本身时，无数据 + 短超时应让 Read 返回超时错误。
func TestDeadlineReaderTimesOutWhenNoData(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			defer c.Close()
			select {} // 服务端永不发数据
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	s := &Smb{conn: conn, timeout: 80 * time.Millisecond}
	r := &deadlineReader{Reader: conn, s: s}
	buf := make([]byte, 4)
	start := time.Now()
	_, err = r.Read(buf)
	if err == nil {
		t.Fatal("read succeeded, want deadline exceeded")
	}
	if ne, ok := err.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("read blocked %v, deadline not applied", elapsed)
	}
}

// timeout = 0 时不设 deadline：无数据时 Read 一直阻塞。
func TestDeadlineReaderDisabledWhenTimeoutZero(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			defer c.Close()
			select {}
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	s := &Smb{conn: conn, timeout: 0}
	r := &deadlineReader{Reader: conn, s: s}
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := r.Read(buf)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		t.Fatalf("read returned %v, want still blocking (no deadline set)", err)
	case <-time.After(150 * time.Millisecond):
		// 仍在阻塞：符合预期
	}
	conn.Close() // 清理，让 goroutine 退出
}

// 有数据可读时 Read 正常返回，且 deadline 被续期（下轮 Read 仍能读到后续数据）。
func TestDeadlineReaderPassesDataThrough(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		c2.Write([]byte("hello"))
	}()
	s := &Smb{conn: c1, timeout: 5 * time.Second}
	r := &deadlineReader{Reader: c1, s: s}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q, want hello", string(buf))
	}
}

// deadlineWriter：对端不读时写阻塞，超时后 Write 返回超时错误。
func TestDeadlineWriterTimesOutWhenStalled(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	// c2 无人读，Write 会阻塞
	s := &Smb{conn: c1, timeout: 80 * time.Millisecond}
	w := &deadlineWriter{Writer: c1, s: s}
	start := time.Now()
	_, err := w.Write([]byte("xxxxxxxx"))
	if err == nil {
		t.Fatal("write succeeded, want deadline exceeded")
	}
	if ne, ok := err.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("write blocked %v, deadline not applied", elapsed)
	}
}

// Write 成功后 deadline 立即清除：之后的裸 Read 不受残留 deadline 影响。
func TestDeadlineWriterClearsDeadlineAfterWrite(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		buf := make([]byte, 8)
		io.ReadFull(c2, buf)
	}()
	s := &Smb{conn: c1, timeout: 50 * time.Millisecond}
	w := &deadlineWriter{Writer: c1, s: s}
	if _, err := w.Write([]byte("xxxxxxxx")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 裸 Read（不经 wrapper）：若残留 deadline，会在 50ms 内失败
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := c1.Read(buf)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		t.Fatalf("read returned %v, want blocking (deadline cleared)", err)
	case <-time.After(150 * time.Millisecond):
		// 仍阻塞：deadline 已清除，符合预期
	}
	c1.Close()
}

// timeout = 0 时 deadlineWriter 完全不设 deadline：写正常完成。
func TestDeadlineWriterDisabledWhenTimeoutZero(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		buf := make([]byte, 4)
		io.ReadFull(c2, buf)
	}()
	s := &Smb{conn: c1, timeout: 0}
	w := &deadlineWriter{Writer: c1, s: s}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatalf("write: %v", err)
	}
}
