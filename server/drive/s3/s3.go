package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// listPageSize > 0 时覆盖单页条数，测试用它强制分页
var listPageSize int

type S3 struct {
	cli           *s3.Client
	presigner     *s3.PresignClient
	uploader      *manager.Uploader
	bucket        string
	root          string
	follow302     bool
	force302HTTPS bool
}

func NewS3(endpoint, region, accessKeyID, secretAccessKey, bucket, root string, pathStyle, follow302, force302HTTPS bool) (*S3, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	}
	if endpoint != "" {
		opts = append(opts, config.WithBaseEndpoint(endpoint))
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	cli := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = pathStyle
	})
	return &S3{
		cli:           cli,
		presigner:     s3.NewPresignClient(cli),
		uploader:      manager.NewUploader(cli),
		bucket:        bucket,
		root:          strings.Trim(root, "/"),
		follow302:     follow302,
		force302HTTPS: force302HTTPS,
	}, nil
}

func (d *S3) key(p string) string {
	// Windows 上上游 genPath 会产出反斜杠，S3 key 必须是 /
	return path.Join(d.root, filepath.ToSlash(p))
}

// Ping 用列目录探测连通性：OpenList/gofakes3 类服务端不支持 HEAD（坑 5）
func (d *S3) Ping(ctx context.Context) error {
	_, err := d.cli.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(d.bucket),
		Prefix:  aws.String(d.prefix("")),
		MaxKeys: aws.Int32(1),
	})
	return err
}

func (d *S3) Upload(p string, r io.ReadCloser, _ int64, _ time.Time) error {
	defer r.Close()
	_, err := d.uploader.Upload(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
		Body:   r,
	})
	return err
}

func (d *S3) IsExist(p string) (bool, error) {
	key := d.key(p)
	// OpenList/AList 不支持 HEAD（403），follow302 开启时用 list 探测
	if d.follow302 {
		out, err := d.cli.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
			Bucket:  aws.String(d.bucket),
			Prefix:  aws.String(key),
			MaxKeys: aws.Int32(1),
		})
		if err != nil {
			return false, err
		}
		return len(out.Contents) > 0 && aws.ToString(out.Contents[0].Key) == key, nil
	}
	_, err := d.cli.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return false, nil
		}
	}
	return false, err
}

func (d *S3) Download(p string) (io.ReadCloser, int64, error) {
	return d.download(context.Background(), p, 0)
}

func (d *S3) DownloadWithOffset(p string, offset int64) (io.ReadCloser, int64, error) {
	return d.download(context.Background(), p, offset)
}

func (d *S3) download(ctx context.Context, p string, offset int64) (io.ReadCloser, int64, error) {
	if d.follow302 {
		return d.downloadFollow302(ctx, p, offset)
	}
	in := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
	}
	if offset > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	out, err := d.cli.GetObject(ctx, in)
	if err != nil {
		return nil, 0, err
	}
	size := aws.ToInt64(out.ContentLength)
	if out.ContentRange != nil {
		if total, ok := parseTotal(aws.ToString(out.ContentRange)); ok {
			size = total
		}
	}
	return out.Body, size, nil
}

// downloadFollow302 手动两步跟随：预签名 GET（不签 Range，坑 4）→ 302 Location → 直链只带 Range（坑 3）
func (d *S3) downloadFollow302(ctx context.Context, p string, offset int64) (io.ReadCloser, int64, error) {
	po, err := d.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
	})
	if err != nil {
		return nil, 0, err
	}
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer cli.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, po.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	rsp, err := cli.Do(req)
	if err != nil {
		return nil, 0, err
	}

	switch rsp.StatusCode {
	case http.StatusOK: // 服务端开了 web_proxy，直接中转
		return d.offsetBody(rsp, offset)
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		rsp.Body.Close()
		return nil, 0, fmt.Errorf("s3 download %s: unexpected status %d", p, rsp.StatusCode)
	}

	loc := rsp.Header.Get("Location")
	rsp.Body.Close()
	if loc == "" {
		return nil, 0, fmt.Errorf("s3 download %s: %d without Location", p, rsp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		return nil, 0, fmt.Errorf("s3 download %s: bad Location %q: %w", p, loc, err)
	}
	if d.force302HTTPS && u.Scheme != "https" {
		return nil, 0, fmt.Errorf("s3 download %s: redirect Location is not https: %s", p, loc)
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, 0, err
	}
	if offset > 0 {
		req2.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	rsp2, err := cli.Do(req2)
	if err != nil {
		return nil, 0, err
	}
	if rsp2.StatusCode != http.StatusOK && rsp2.StatusCode != http.StatusPartialContent {
		rsp2.Body.Close()
		return nil, 0, fmt.Errorf("s3 direct download %s: unexpected status %d", p, rsp2.StatusCode)
	}
	return d.offsetBody(rsp2, offset)
}

// offsetBody 返回响应流与总大小；服务端忽略 Range 时本地丢弃前 offset 字节
func (d *S3) offsetBody(rsp *http.Response, offset int64) (io.ReadCloser, int64, error) {
	length := rsp.ContentLength
	if rsp.StatusCode == http.StatusPartialContent {
		if total, ok := parseTotal(rsp.Header.Get("Content-Range")); ok {
			return rsp.Body, total, nil
		}
		return rsp.Body, length + offset, nil
	}
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, rsp.Body, offset); err != nil {
			rsp.Body.Close()
			return nil, 0, err
		}
	}
	if length < 0 {
		// 上游 http.go 会把 size 直接写进 Content-Length，-1 会毁掉响应
		// shortcut: 无 Content-Length 的分块直链直接报错，等真遇到这类服务端再改成流式透传
		rsp.Body.Close()
		return nil, 0, fmt.Errorf("s3 download: response has no Content-Length")
	}
	return rsp.Body, length, nil
}

// parseTotal 解析 "bytes 0-99/12345" 中的总长度
func parseTotal(contentRange string) (int64, bool) {
	i := strings.LastIndex(contentRange, "/")
	if i < 0 {
		return 0, false
	}
	total, err := strconv.ParseInt(contentRange[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return total, true
}

func (d *S3) Delete(p string) error {
	_, err := d.cli.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
	})
	return err
}

// prefix 把目录转成列举前缀，非空时保证以 / 结尾
func (d *S3) prefix(dir string) string {
	dir = strings.Trim(filepath.ToSlash(dir), "/")
	if dir == "." { // imgmanager 用 "." 表示根目录
		dir = ""
	}
	full := path.Join(d.root, dir)
	if full == "" {
		return ""
	}
	return full + "/"
}

func (d *S3) Range(dir string, deal func(fs.FileInfo) bool) error {
	prefix := d.prefix(dir)
	pg := s3.NewListObjectsV2Paginator(d.cli, &s3.ListObjectsV2Input{
		Bucket:    aws.String(d.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
		MaxKeys:   pageSize(),
	})
	var infos []fs.FileInfo
	for pg.HasMorePages() {
		out, err := pg.NextPage(context.Background())
		if err != nil {
			return err
		}
		for _, cp := range out.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), prefix), "/")
			infos = append(infos, &fileInfo{name: name, dir: true})
		}
		for _, obj := range out.Contents {
			k := aws.ToString(obj.Key)
			if strings.HasSuffix(k, "/") { // 伪目录 key，跳过
				continue
			}
			infos = append(infos, &fileInfo{
				name: strings.TrimPrefix(k, prefix),
				size: aws.ToInt64(obj.Size),
				mod:  aws.ToTime(obj.LastModified),
			})
		}
	}
	sort.Sort(desc(infos))
	dealAll(infos, deal)
	return nil
}

// dealAll 按顺序回调，返回 false 表示调用方要求停止
func dealAll(infos []fs.FileInfo, deal func(fs.FileInfo) bool) bool {
	for _, fi := range infos {
		if !deal(fi) {
			return false
		}
	}
	return true
}

func pageSize() *int32 {
	if listPageSize > 0 {
		return aws.Int32(int32(listPageSize))
	}
	return nil
}

type fileInfo struct {
	name string
	size int64
	mod  time.Time
	dir  bool
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() fs.FileMode  { return 0644 }
func (f *fileInfo) ModTime() time.Time { return f.mod }
func (f *fileInfo) IsDir() bool        { return f.dir }
func (f *fileInfo) Sys() any           { return nil }

type desc []fs.FileInfo

func (d desc) Len() int           { return len(d) }
func (d desc) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }
func (d desc) Less(i, j int) bool { return d[i].ModTime().After(d[j].ModTime()) }

func (d *S3) Close() error { return nil }
