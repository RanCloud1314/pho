package s3

import (
	"bytes"
	"io"
	"testing"
)

func TestSemBodyCloseReleasesOnce(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // 占用唯一槽位
	b := &semBody{rc: io.NopCloser(bytes.NewReader([]byte("x"))), sem: sem}
	p := make([]byte, 1)
	if _, err := b.Read(p); err != nil || p[0] != 'x' {
		t.Fatal("Read 未透传")
	}
	b.Close()
	b.Close() // 双 Close 不得二次释放
	if len(sem) != 0 {
		t.Fatalf("sem len = %d, want 0", len(sem))
	}
	select {
	case sem <- struct{}{}: // 槽位已释放，可重新占用
	default:
		t.Fatal("Close 后槽位未释放")
	}
}

func TestDownloadErrorReleasesSlot(t *testing.T) {
	// 端口 1 上无服务：连接拒绝，快速失败
	d, err := NewS3("http://127.0.0.1:1", "us-east-1", "ak", "sk", "b", "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Download("x.jpg"); err == nil {
		t.Fatal("want error")
	}
	if len(d.sem) != 0 {
		t.Fatal("错误路径 sem 槽位泄漏")
	}
}
