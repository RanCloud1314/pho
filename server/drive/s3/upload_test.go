package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
)

func TestUploadProgressSinglePut(t *testing.T) {
	bodyStarted := make(chan struct{})
	readRest := make(chan struct{})
	bodyReceived := make(chan struct{})
	reply := make(chan struct{})
	var allowRead, allowReply sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(io.Discard, r.Body, 64<<10)
		close(bodyStarted)
		<-readRest
		_, _ = io.Copy(io.Discard, r.Body)
		close(bodyReceived)
		<-reply
		w.Header().Set("ETag", `"put"`)
	}))
	defer server.Close()
	defer allowReply.Do(func() { close(reply) })
	defer allowRead.Do(func() { close(readRest) })
	drive := uploadTestDrive(t, server.URL)
	ctx, progress := TrackUpload(context.Background())
	const size = 4 * 1024 * 1024
	done := make(chan error, 1)
	go func() {
		done <- drive.UploadContext(ctx, "test.jpg", io.NopCloser(bytes.NewReader(make([]byte, size))), size, time.Now())
	}()
	waitUploadTest(t, bodyStarted)
	if s := progress.Snapshot(); s.Total != size || s.Confirmed != 0 {
		t.Fatalf("single upload must have indeterminate progress: %+v", s)
	}
	allowRead.Do(func() { close(readRest) })
	waitUploadTest(t, bodyReceived)
	if s := progress.Snapshot(); s.Phase != "confirming" || s.Rate != 0 {
		t.Fatalf("expected confirmation without speed: %+v", s)
	}
	allowReply.Do(func() { close(reply) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUploadMultipartProgressAndInvalidPart(t *testing.T) {
	complete := make(chan struct{})
	reply := make(chan struct{})
	var allowReply sync.Once
	var firstPartAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		query := r.URL.Query()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case query.Has("uploads"):
			fmt.Fprint(w, "<InitiateMultipartUploadResult><UploadId>test-id</UploadId></InitiateMultipartUploadResult>")
		case r.Method == http.MethodPut:
			if query.Get("partNumber") == "1" && firstPartAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("ETag", `"part"`)
		case r.Method == http.MethodPost:
			close(complete)
			<-reply
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<Error><Code>InvalidPart</Code><Message>unexpected part number 1 in complete request</Message></Error>")
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	defer allowReply.Do(func() { close(reply) })
	drive := uploadTestDrive(t, server.URL)
	ctx, progress := TrackUpload(context.Background())
	size := manager.DefaultUploadPartSize + 1234
	done := make(chan error, 1)
	go func() {
		done <- drive.UploadContext(ctx, "test.jpg", io.NopCloser(bytes.NewReader(make([]byte, size))), size, time.Now())
	}()
	waitUploadTest(t, complete)
	if s := progress.Snapshot(); s.Phase != "confirming" || s.Confirmed != size || s.Rate != 0 {
		t.Fatalf("expected each successful part counted once: %+v", s)
	}
	if attempts := firstPartAttempts.Load(); attempts != 2 {
		t.Fatalf("first part attempts = %d", attempts)
	}
	allowReply.Do(func() { close(reply) })
	if err := <-done; err == nil || !strings.Contains(err.Error(), `S3 CompleteMultipartUpload HTTP 400 ("InvalidPart")`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUploadHTML403(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><head><meta></head></html>")
	}))
	defer server.Close()
	err := uploadTestDrive(t, server.URL).Upload("test.jpg", io.NopCloser(strings.NewReader("photo")), 5, time.Now())
	if err == nil || err.Error() != "S3 PutObject HTTP 403" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUploadRateUsesTransportReads(t *testing.T) {
	_, progress := TrackUpload(context.Background())
	body := &uploadBody{
		ReadCloser: io.NopCloser(strings.NewReader("photo")),
		progress:   progress,
		length:     5,
	}
	if rate := progress.Snapshot().Rate; rate != 0 {
		t.Fatalf("rate without sends = %f", rate)
	}
	if _, err := body.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	progress.sampledAt = time.Now().Add(-time.Second)
	if rate := progress.Snapshot().Rate; rate <= 0 {
		t.Fatalf("rate after transport read = %f", rate)
	}
	if rate := progress.Snapshot().Rate; rate != 0 {
		t.Fatalf("rate without additional writes = %f", rate)
	}
}

func uploadTestDrive(t *testing.T, endpoint string) *S3 {
	t.Helper()
	drive, err := NewS3(endpoint, "us-east-1", "test", "test", "bucket", "", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return drive
}

func waitUploadTest(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for upload")
	}
}
