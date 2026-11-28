package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fregie/img_syncer/server/drive/s3"
	"github.com/fregie/img_syncer/server/imgmanager"
)

func TestHTTPUploadProgressLifetime(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(arrived)
		<-release
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><head><meta></head></html>")
	}))
	defer upstream.Close()
	defer unblock()
	drive, err := s3.NewS3(upstream.URL, "us-east-1", "test", "test", "bucket", "", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAPI()
	defer a.im.Close()
	defer unblock()
	a.im.SetDrive(drive)
	const id = "0123456789abcdef0123456789abcdef"
	post := httptest.NewRequest(http.MethodPost, "/test.jpg", bytes.NewBufferString("photo"))
	post.RemoteAddr = "127.0.0.1:1234"
	post.Header.Set("Image-Upload-ID", id)
	result := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		a.HttpHandler().ServeHTTP(result, post)
		close(done)
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("S3 upload did not start")
	}
	get := httptest.NewRequest(http.MethodGet, uploadProgressPath+id, nil)
	get.RemoteAddr = post.RemoteAddr
	sample := httptest.NewRecorder()
	a.HttpHandler().ServeHTTP(sample, get)
	var snapshot s3.UploadSnapshot
	if err := json.Unmarshal(sample.Body.Bytes(), &snapshot); err != nil ||
		sample.Code != http.StatusOK || snapshot.Phase != "confirming" ||
		snapshot.Confirmed != 0 || snapshot.Total != 5 || snapshot.Rate != 0 {
		t.Errorf("unexpected sample: status=%d body=%q error=%v", sample.Code, sample.Body, err)
	}
	unblock()
	<-done
	if result.Code != http.StatusBadRequest || result.Body.String() != "error uploading: S3 PutObject HTTP 403" {
		t.Errorf("unexpected upload result: status=%d body=%q", result.Code, result.Body)
	}
	sample = httptest.NewRecorder()
	a.HttpHandler().ServeHTTP(sample, get)
	if sample.Code != http.StatusNotFound {
		t.Errorf("finished upload progress remains available: %d", sample.Code)
	}
}

// mockDrive 实现 imgmanager.StorageDrive 接口，用于测试 HTTP Range 处理
type mockDrive struct {
	content []byte
}

func (m *mockDrive) Upload(path string, content io.ReadCloser, size int64, modTime time.Time) error {
	return nil
}
func (m *mockDrive) IsExist(path string) (bool, error) { return true, nil }
func (m *mockDrive) Download(path string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(m.content)), int64(len(m.content)), nil
}
func (m *mockDrive) DownloadWithOffset(path string, offset int64) (io.ReadCloser, int64, error) {
	if offset >= int64(len(m.content)) {
		return nil, 0, io.EOF
	}
	return io.NopCloser(bytes.NewReader(m.content[offset:])), int64(len(m.content)), nil
}
func (m *mockDrive) Delete(path string) error                            { return nil }
func (m *mockDrive) Range(dir string, deal func(fs.FileInfo) bool) error { return nil }
func (m *mockDrive) Close() error                                        { return nil }

func newTestAPI() *api {
	im := imgmanager.NewImgManager(imgmanager.Option{WorkerNum: 1})
	im.SetDrive(&mockDrive{content: []byte("0123456789")})
	a := NewApi(im)
	a.SetHttpPort(0)
	return a
}

// newTestAPIWithEncryptedGcm 构造一个 mockDrive 持有 GCM 加密 buffer 的 test API。
func newTestAPIWithEncryptedGcm(password string, plaintext []byte) (*api, []byte) {
	im := imgmanager.NewImgManager(imgmanager.Option{WorkerNum: 1})
	encReader, err := imgmanager.EncryptedReaderWraper(
		io.NopCloser(bytes.NewReader(plaintext)),
		imgmanager.EncryptOption{Type: imgmanager.AES_256_GCM, Password: password},
	)
	if err != nil {
		panic(err)
	}
	encrypted, err := io.ReadAll(encReader)
	if err != nil {
		panic(err)
	}
	encReader.Close()
	im.SetDrive(&mockDrive{content: encrypted})
	a := NewApi(im)
	a.SetHttpPort(0)
	return a, encrypted
}

func doRangeRequest(a *api, rangeHeader string) *httptest.ResponseRecorder {
	return doRangeRequestWith(a, "test.jpg", rangeHeader, "", "")
}

func doRangeRequestWith(a *api, path, rangeHeader, encType, encPassword string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/"+path, nil)
	// 去除前导斜杠，避免 sanitizePath 将路径视为绝对路径而拒绝
	req.URL.Path = path
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	if encType != "" {
		req.Header.Set(HeaderEncryptType, encType)
	}
	if encPassword != "" {
		req.Header.Set(HeaderEncryptPassword, encPassword)
	}
	w := httptest.NewRecorder()
	a.httpHandler(w, req)
	return w
}

func TestHTTPDownloadRangeNormalPass(t *testing.T) {
	a := newTestAPI()

	// 测试 bytes=start-end 格式
	w := doRangeRequest(a, "bytes=0-4")
	if w.Code != http.StatusPartialContent {
		t.Fatalf("bytes=0-4: expected 206, got %d", w.Code)
	}
	if w.Body.String() != "01234" {
		t.Fatalf("bytes=0-4: expected body '01234', got '%s'", w.Body.String())
	}

	// 测试 bytes=start- 格式（无 end）
	w2 := doRangeRequest(a, "bytes=5-")
	if w2.Code != http.StatusPartialContent {
		t.Fatalf("bytes=5-: expected 206, got %d", w2.Code)
	}
	if w2.Body.String() != "56789" {
		t.Fatalf("bytes=5-: expected body '56789', got '%s'", w2.Body.String())
	}
}

func TestHTTPDownloadRangeNegativeRejected(t *testing.T) {
	a := newTestAPI()

	tests := []struct {
		name        string
		rangeHeader string
	}{
		{"start negative", "bytes=-50"},
		{"both negative", "bytes=-50-100"},
		{"suffix range form", "bytes=-500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doRangeRequest(a, tt.rangeHeader)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d", tt.name, w.Code)
			}
		})
	}
}

func TestHTTPDownloadRangeOnGcmEncrypted(t *testing.T) {
	password := "gcm-range-pw"
	// 2.5MB plaintext 跨多个 GCM 分块（chunkSize=1MB）
	plaintext := make([]byte, 2*1024*1024+500*1024)
	for i := range plaintext {
		plaintext[i] = byte(i % 251)
	}

	a, _ := newTestAPIWithEncryptedGcm(password, plaintext)
	w := doRangeRequestWith(a, "test.jpg.aes", "bytes=1000000-1500000", "AES_256_GCM", password)
	if w.Code != http.StatusPartialContent {
		bodyPreview := w.Body.Bytes()
		if len(bodyPreview) > 64 {
			bodyPreview = bodyPreview[:64]
		}
		t.Fatalf("expected 206, got %d (body preview=%q)", w.Code, bodyPreview)
	}
	cr := w.Header().Get("Content-Range")
	wantCR := "bytes 1000000-1500000/2609152"
	if cr != wantCR {
		t.Fatalf("Content-Range: got %q, want %q", cr, wantCR)
	}
	want := plaintext[1000000 : 1500000+1]
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("body mismatch: got %d bytes, want %d bytes", w.Body.Len(), len(want))
	}
}

func TestHTTPDownloadRangeGcmZeroStart(t *testing.T) {
	password := "gcm-range-pw-zero"
	plaintext := make([]byte, 2*1024*1024+500*1024)
	for i := range plaintext {
		plaintext[i] = byte(i % 251)
	}

	a, _ := newTestAPIWithEncryptedGcm(password, plaintext)
	w := doRangeRequestWith(a, "test.jpg.aes", "bytes=0-", "AES_256_GCM", password)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d (body len=%d)", w.Code, w.Body.Len())
	}
	cr := w.Header().Get("Content-Range")
	wantCR := "bytes 0-2609151/2609152"
	if cr != wantCR {
		t.Fatalf("Content-Range: got %q, want %q", cr, wantCR)
	}
	if !bytes.Equal(w.Body.Bytes(), plaintext) {
		t.Fatalf("body mismatch: got %d bytes, want %d bytes", w.Body.Len(), len(plaintext))
	}
}
