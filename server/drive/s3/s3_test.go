package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/fregie/img_syncer/test/static"
)

// 集成测试依赖任意 S3 兼容服务端（docker-compose 中的 minio，或本地 s3rver）。
// 未设置 S3_TEST_ENDPOINT 时跳过。
type S3TestSuite struct {
	suite.Suite
	endpoint string
	region   string
	ak       string
	sk       string
	bucket   string
}

func TestS3DriveSuite(t *testing.T) {
	suite.Run(t, new(S3TestSuite))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *S3TestSuite) SetupSuite() {
	s.endpoint = os.Getenv("S3_TEST_ENDPOINT")
	if s.endpoint == "" {
		s.T().Skip("S3_TEST_ENDPOINT not set; skipping s3 drive integration tests")
	}
	s.region = envOr("S3_TEST_REGION", "us-east-1")
	s.ak = envOr("S3_TEST_AK", "fregie")
	s.sk = envOr("S3_TEST_SK", "password123")
	s.bucket = envOr("S3_TEST_BUCKET", "pho-test")

	d, err := NewS3(s.endpoint, s.region, s.ak, s.sk, s.bucket, "", true, false, false)
	s.Require().NoError(err)
	_, err = d.cli.CreateBucket(s.T().Context(), &awss3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") && !strings.Contains(err.Error(), "BucketAlreadyExists") {
		s.Require().NoError(err)
	}
}

func (s *S3TestSuite) newDrive(root string) *S3 {
	d, err := NewS3(s.endpoint, s.region, s.ak, s.sk, s.bucket, root, true, false, false)
	s.Require().NoError(err)
	return d
}

func (s *S3TestSuite) upload(d *S3, key string) {
	err := d.Upload(key, io.NopCloser(bytes.NewReader(static.Pic1)), int64(len(static.Pic1)), time.Now())
	s.Require().NoError(err)
}

func (s *S3TestSuite) TestUploadDownloadRoundtrip() {
	d := s.newDrive("")
	key := "2026/11/25/20060102030405_test_pic_01.jpg"
	s.upload(d, key)

	exist, err := d.IsExist(key)
	s.Require().NoError(err)
	s.True(exist)

	rc, size, err := d.Download(key)
	s.Require().NoError(err)
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	s.Equal(int64(len(static.Pic1)), size)
	s.Equal(static.Pic1, b)

	s.Require().NoError(d.Delete(key))
	exist, err = d.IsExist(key)
	s.Require().NoError(err)
	s.False(exist)
}

func (s *S3TestSuite) TestIsExistFollow302() {
	// follow302 开启时走 list 探测（OpenList 不支持 HEAD）
	d, err := NewS3(s.endpoint, s.region, s.ak, s.sk, s.bucket, "", true, true, false)
	s.Require().NoError(err)
	key := "ex302/2026/11/25/a.jpg"
	s.upload(d, key)

	exist, err := d.IsExist(key)
	s.Require().NoError(err)
	s.True(exist)

	// 前缀相同但不是同一对象
	exist, err = d.IsExist("ex302/2026/11/25/b.jpg")
	s.Require().NoError(err)
	s.False(exist)
}

func (s *S3TestSuite) TestRootPrefix() {
	d := s.newDrive("photos")
	key := "2026/11/25/a.jpg"
	s.upload(d, key)

	// 实际对象 key 带 root 前缀
	_, err := d.cli.HeadObject(s.T().Context(), &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("photos/" + key),
	})
	s.Require().NoError(err)

	var files []string
	err = d.Range("2026/11/25", func(fi fs.FileInfo) bool { files = append(files, fi.Name()); return true })
	s.Require().NoError(err)
	s.Equal([]string{"a.jpg"}, files)
}

func (s *S3TestSuite) TestDownloadWithOffset() {
	d := s.newDrive("")
	key := "2026/11/25/offset.jpg"
	s.upload(d, key)

	rc, size, err := d.DownloadWithOffset(key, 5)
	s.Require().NoError(err)
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	s.Equal(static.Pic1[5:], b)
	s.Equal(int64(len(static.Pic1)), size)
}

func (s *S3TestSuite) TestConcurrentDownload() {
	d := s.newDrive("")
	key := "2026/11/25/concurrent.jpg"
	s.upload(d, key)

	// 并发 20 > 上限 8：若 semBody 的 Close 不释放槽位，后续批次会
	// 永久阻塞、测试超时失败；内容一致性同时验证分片拼接无串扰。
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := d.Download(key)
			if err != nil {
				errs <- err
				return
			}
			b, err := io.ReadAll(rc)
			rc.Close() // 槽位在此释放
			if err == nil && !bytes.Equal(static.Pic1, b) {
				err = fmt.Errorf("content mismatch")
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		s.Require().NoError(err)
	}
}

func (s *S3TestSuite) TestRangeDirs() {
	d := s.newDrive("")
	s.upload(d, "rd/2026/11/25/a.jpg")
	s.upload(d, "rd/2026/12/26/b.jpg")

	var dirs, files []string
	err := d.Range("rd/2026/", func(fi fs.FileInfo) bool {
		if fi.IsDir() {
			dirs = append(dirs, fi.Name())
		} else {
			files = append(files, fi.Name())
		}
		return true
	})
	s.Require().NoError(err)
	s.Empty(files)
	s.ElementsMatch([]string{"11", "12"}, dirs)
}

func (s *S3TestSuite) TestPagination() {
	old := listPageSize
	listPageSize = 2
	defer func() { listPageSize = old }()

	d := s.newDrive("")
	for i := 0; i < 3; i++ {
		s.upload(d, fmt.Sprintf("pg/2026/11/25/p%d.jpg", i))
	}

	var files []string
	err := d.Range("pg/2026/11/25/", func(fi fs.FileInfo) bool {
		files = append(files, fi.Name())
		return true
	})
	s.Require().NoError(err)
	s.Len(files, 3)

	// deal 返回 false 提前终止
	calls := 0
	err = d.Range("pg/2026/11/25/", func(fi fs.FileInfo) bool {
		calls++
		return false
	})
	s.Require().NoError(err)
	s.Equal(1, calls)
}

// 302 直链跟随：用两个 httptest 服务模拟 OpenList + 直链存储。不依赖外部服务。
func TestFollow302Download(t *testing.T) {
	var directHit http.Header
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHit = r.Header.Clone()
		w.Header().Set("Content-Length", strconv.Itoa(len(static.Pic1)))
		if r.Header.Get("Range") != "" {
			start := 0
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(static.Pic1)-1, len(static.Pic1)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(static.Pic1[start:])
			return
		}
		w.Write(static.Pic1)
	}))
	defer direct.Close()

	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// 坑 4：带 Range 的签名请求应被拒绝
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		w.Header().Set("Location", direct.URL+"/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer openlist.Close()

	d, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)

	rc, size, err := d.Download("2026/11/25/a.jpg")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Equal(t, static.Pic1, b)
	require.Equal(t, int64(len(static.Pic1)), size)

	rc, size, err = d.DownloadWithOffset("2026/11/25/a.jpg", 5)
	require.NoError(t, err)
	b, _ = io.ReadAll(rc)
	rc.Close()
	require.Equal(t, static.Pic1[5:], b)
	require.Equal(t, int64(len(static.Pic1)), size)
	require.Equal(t, "bytes=5-", directHit.Get("Range"))
	require.Empty(t, directHit.Get("Authorization"), "direct link must not carry Authorization")
}

func TestFollow302RangeHonoredByServer(t *testing.T) {
	// 标准 S3/R2/MinIO 接受签名 Range，不该整文件重下
	var gotRange string
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if gotRange == "" {
			redirected = true
			w.Header().Set("Location", "http://127.0.0.1:1/nope")
			w.WriteHeader(http.StatusFound)
			return
		}
		start := 0
		fmt.Sscanf(gotRange, "bytes=%d-", &start)
		w.Header().Set("Content-Length", strconv.Itoa(len(static.Pic1)-start))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(static.Pic1)-1, len(static.Pic1)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(static.Pic1[start:])
	}))
	defer server.Close()

	d, err := NewS3(server.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)
	rc, size, err := d.DownloadWithOffset("a.jpg", 7)
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Equal(t, static.Pic1[7:], b)
	require.Equal(t, int64(len(static.Pic1)), size)
	require.Equal(t, "bytes=7-", gotRange)
	require.False(t, redirected, "ranged GET succeeded, no 302 hop expected")
}

func TestNormalizeEndpoint(t *testing.T) {
	require.Equal(t, "https://minio.local:9000", normalizeEndpoint("minio.local:9000"))
	require.Equal(t, "http://127.0.0.1:9000", normalizeEndpoint("http://127.0.0.1:9000"))
	require.Equal(t, "", normalizeEndpoint(""))
}

// offset == 文件大小（已下完）时直链回 416，应当当空 body 处理
func TestFollow302OffsetAtEOF(t *testing.T) {
	total := len(static.Pic1)
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer direct.Close()
	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", direct.URL+"/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer openlist.Close()

	d, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)
	rc, size, err := d.DownloadWithOffset("a.jpg", int64(total))
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Empty(t, b)
	require.Equal(t, int64(total), size)
}

func TestFollow302ForceHttps(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(static.Pic1)))
		w.Write(static.Pic1)
	}))
	defer plain.Close()

	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", plain.URL+"/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer openlist.Close()

	strict, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, true)
	require.NoError(t, err)
	_, _, err = strict.Download("a.jpg")
	require.Error(t, err)
	require.Contains(t, err.Error(), "https")

	loose, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)
	rc, _, err := loose.Download("a.jpg")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Equal(t, static.Pic1, b)
}

// 直链既无 Content-Length 又走分块编码时必须报错：上游会把 size 直接写进 Content-Length
func TestFollow302ChunkedDirectLink(t *testing.T) {
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(static.Pic1) // >2KB 且未设置长度 -> chunked, ContentLength = -1
	}))
	defer chunked.Close()

	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", chunked.URL+"/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer openlist.Close()

	d, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)
	_, _, err = d.Download("a.jpg")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Content-Length")
}

// —— 并行分片下载 ——
// 根因：冷启动首张原图十几秒 = 单连接 TCP 慢启动；分片多连接并行取回。
// 首片 Range bytes=0-2M 的 206 Content-Range 给出总大小，据此决定单流或分片。

// patternContent 生成确定性伪随机内容
func patternContent(n int) []byte {
	b := make([]byte, n)
	x := uint32(123456789)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

type rangeCounter struct {
	mu     sync.Mutex
	gets   int
	ranged int
}

func (c *rangeCounter) record(rng string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if rng != "" {
		c.ranged++
	}
}

func (c *rangeCounter) snapshot() (gets, ranged int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.ranged
}

// newRangeS3Server 模拟支持 Range 的 S3 GetObject（path-style）
func newRangeS3Server(t *testing.T, content []byte) (*httptest.Server, *rangeCounter) {
	c := &rangeCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		c.record(rng)
		start, end := int64(0), int64(len(content)-1)
		if rng != "" {
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		}
		w.Header().Set("Content-Length", strconv.Itoa(int(end-start+1)))
		if rng != "" {
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		w.Write(content[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestParallelDownloadSDK(t *testing.T) {
	content := patternContent(5 << 20) // 5MB：首片 2MB + 尾片 3MB
	srv, c := newRangeS3Server(t, content)
	d, err := NewS3(srv.URL, "us-east-1", "ak", "sk", "bucket", "", true, false, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	require.Equal(t, content, b)
	require.Equal(t, int64(len(content)), size)
	gets, ranged := c.snapshot()
	require.GreaterOrEqual(t, ranged, 3, "窗口内对象应当并行分片")
	require.Equal(t, gets, ranged, "分片路径全部带 Range")
}

func TestParallelDownloadLargeFallsBack(t *testing.T) {
	content := patternContent(33 << 20) // >32MB：退回单流，不整体缓冲
	srv, c := newRangeS3Server(t, content)
	d, err := NewS3(srv.URL, "us-east-1", "ak", "sk", "bucket", "", true, false, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	require.Equal(t, content, b)
	require.Equal(t, int64(len(content)), size)
	gets, ranged := c.snapshot()
	require.Equal(t, 2, gets, "大文件：首片探测 + 单流全量")
	require.Equal(t, 1, ranged)
}

func TestParallelDownloadRangeIgnored(t *testing.T) {
	content := patternContent(3 << 20)
	c := &rangeCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r.Header.Get("Range"))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Write(content) // 无视 Range，整文件 200
	}))
	defer srv.Close()
	d, err := NewS3(srv.URL, "us-east-1", "ak", "sk", "bucket", "", true, false, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	require.Equal(t, content, b)
	require.Equal(t, int64(len(content)), size)
	gets, _ := c.snapshot()
	require.Equal(t, 1, gets, "服务端忽略 Range 时应当单请求流式")
}

func TestParallelDownloadEmptyObject(t *testing.T) {
	c := &rangeCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		c.record(rng)
		if rng != "" { // S3 对空对象 + Range 返回 416 InvalidRange
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			fmt.Fprint(w, `<Error><Code>InvalidRange</Code><Message>range not satisfiable</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.Write(nil)
	}))
	defer srv.Close()
	d, err := NewS3(srv.URL, "us-east-1", "ak", "sk", "bucket", "", true, false, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Empty(t, b)
	require.Equal(t, int64(0), size)
}

func TestParallelDownloadFollow302(t *testing.T) {
	content := patternContent(5 << 20)
	c := &rangeCounter{}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("direct link must not carry Authorization")
		}
		rng := r.Header.Get("Range")
		c.record(rng)
		start, end := int64(0), int64(len(content)-1)
		if rng != "" {
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		}
		w.Header().Set("Content-Length", strconv.Itoa(int(end-start+1)))
		if rng != "" {
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		w.Write(content[start : end+1])
	}))
	defer direct.Close()

	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" { // 坑 4：带 Range 的签名请求被拒绝
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Location", direct.URL+"/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer openlist.Close()

	d, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	require.Equal(t, content, b)
	require.Equal(t, int64(len(content)), size)
	_, ranged := c.snapshot()
	require.GreaterOrEqual(t, ranged, 3, "直链上应当并行分片")
}

func TestFollow302EmptyObject(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes */0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", "0")
		w.Write(nil)
	}))
	defer direct.Close()

	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", direct.URL+"/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer openlist.Close()

	d, err := NewS3(openlist.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Empty(t, b)
	require.Equal(t, int64(0), size)
}

// web_proxy 中转：预签名 GET 直接 200 分块（无 Content-Length），list 仍可用——用 list 探测真实大小
func TestWebProxyChunkedRelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" { // ListObjectsV2
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><Contents><Key>a.jpg</Key><Size>%d</Size></Contents></ListBucketResult>`, len(static.Pic1))
			return
		}
		if r.Header.Get("Range") != "" { // 坑 4：OpenList 拒绝带 Range 的签名请求
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		w.WriteHeader(http.StatusOK) // 中转：200 分块，无 Content-Length
		w.Write(static.Pic1)
	}))
	defer srv.Close()

	d, err := NewS3(srv.URL, "us-east-1", "ak", "sk", "bucket", "", true, true, false)
	require.NoError(t, err)

	rc, size, err := d.Download("a.jpg")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	rc.Close()
	require.Equal(t, static.Pic1, b)
	require.Equal(t, int64(len(static.Pic1)), size)

	rc, size, err = d.DownloadWithOffset("a.jpg", 5)
	require.NoError(t, err)
	b, _ = io.ReadAll(rc)
	rc.Close()
	require.Equal(t, static.Pic1[5:], b)
	require.Equal(t, int64(len(static.Pic1)), size)
}

func TestAlbumIndexReusesListAndInvalidates(t *testing.T) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		lists.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>album/20261128/a.jpg</Key><Size>9</Size></Contents>`+
			`<Contents><Key>album/20261128/live_b/b.jpg</Key><Size>10</Size></Contents>`+
			`<Contents><Key>album/.thumbnail/20261128/a.jpg</Key><Size>1</Size></Contents>`+
			`</ListBucketResult>`)
	}))
	defer srv.Close()

	d, err := NewS3(srv.URL, "us-east-1", "key", "secret", "photos", "album", true, false, false)
	require.NoError(t, err)
	require.NoError(t, d.PrepareAlbumIndex(t.Context()))
	require.NoError(t, d.PrepareAlbumIndex(t.Context()))
	var names []string
	require.NoError(t, d.Range("20261128", func(info fs.FileInfo) bool {
		names = append(names, info.Name())
		return true
	}))
	require.ElementsMatch(t, []string{"a.jpg", "live_b"}, names)
	require.EqualValues(t, 1, lists.Load())
	require.NoError(t, d.Delete("20261128/a.jpg"))
	require.NoError(t, d.PrepareAlbumIndex(t.Context()))
	require.EqualValues(t, 2, lists.Load())
}

func TestBoundedDownloadUsesExactRange(t *testing.T) {
	srv, _ := newRangeS3Server(t, []byte("0123456789"))
	defer srv.Close()
	d, err := NewS3(srv.URL, "us-east-1", "key", "secret", "photos", "", true, false, false)
	require.NoError(t, err)
	body, size, err := d.DownloadMedia(t.Context(), "movie.mp4", 0, 3)
	require.NoError(t, err)
	defer body.Close()
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "0123", string(data))
	require.EqualValues(t, 10, size)
}

func TestDownloadCancellationReleasesSlot(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	d, err := NewS3(srv.URL, "us-east-1", "key", "secret", "photos", "", true, false, false)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body, _, err := d.DownloadMedia(ctx, "a.jpg", -1, -1)
	require.NoError(t, err)
	defer body.Close()
	<-started
	cancel()
	_, err = io.ReadAll(body)
	require.Error(t, err)
	require.NoError(t, body.Close())
	require.Empty(t, d.sem)
}
