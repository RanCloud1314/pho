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
