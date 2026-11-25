package s3

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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

func TestFollow302ForceHttps(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
