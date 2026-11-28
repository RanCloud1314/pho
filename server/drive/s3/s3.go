package s3

import (
	"bytes"
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
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkhttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"
)

// listPageSize > 0 时覆盖单页条数，测试用它强制分页
var listPageSize int

// 并行分片下载参数：首张原图冷连接十几秒的根因是单连接 TCP 慢启动，
// 4 条连接并行取回绕开爬坡。
const (
	firstChunkSize    = 2 << 20  // 首片大小，也是分片起点
	tailFloor         = 1 << 20  // 尾片下限，避免为几百 KB 开多条连接
	maxTails          = 3        // 并行尾片数（加上首片共 4 条连接）
	parallelMaxSize   = 32 << 20 // 超过该大小退回单流：整体缓冲不划算（视频等大文件）
	maxConcurrentGets = 8        // GET 并发上限：挡客户端缩略图风暴（spec: 冷启动优化）
)

type S3 struct {
	cli           *s3.Client
	presigner     *s3.PresignClient
	uploader      *manager.Uploader
	httpNoRedir   *http.Client // 第一步：手动看 302
	httpFollow    *http.Client // 第二步：直链可以自己再跳
	bucket        string
	root          string
	follow302     bool
	force302HTTPS bool
	indexMu       sync.Mutex
	index         map[string][]fs.FileInfo
	indexExpires  time.Time
	indexVersion  uint64
	indexLoading  chan struct{}
	sem           chan struct{} // GET 并发槽位，download 时占用、body Close 时释放
}

func NewS3(endpoint, region, accessKeyID, secretAccessKey, bucket, root string, pathStyle, follow302, force302HTTPS bool) (*S3, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	}
	if endpoint != "" {
		opts = append(opts, config.WithBaseEndpoint(normalizeEndpoint(endpoint)))
	}
	// 只限制首字节等待，不限流式 body，否则大视频会被中途掐断
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	// 默认空闲连接每主机仅 2 条，并发 6~8 时反复建拆（spec: 冷启动优化）
	tr.MaxIdleConnsPerHost = 16
	noRedir := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// SDK 客户端（follow302=false 时缩略图全走它）同样修首字节等待与空闲连接数
	opts = append(opts, config.WithHTTPClient(sdkhttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.ResponseHeaderTimeout = 60 * time.Second
		t.MaxIdleConnsPerHost = 16
	})))
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
		httpNoRedir:   noRedir,
		httpFollow:    &http.Client{Transport: tr},
		bucket:        bucket,
		root:          strings.Trim(root, "/"),
		follow302:     follow302,
		force302HTTPS: force302HTTPS,
		sem:           make(chan struct{}, maxConcurrentGets),
	}, nil
}

// normalizeEndpoint 补全 scheme：表单允许只填主机名，恢复配置时可能原样传进来
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" || strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "https://" + endpoint
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

func (d *S3) Upload(p string, r io.ReadCloser, size int64, _ time.Time) error {
	defer r.Close()
	_, err := d.uploader.Upload(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
		Body:   r,
	})
	d.invalidateAlbumIndex()
	if err != nil || !d.follow302 || size <= 0 {
		return err
	}
	// OpenList 前面的 nginx/WAF 可能吞掉 PUT body 仍返回 200（指南坑 6），落盘后核对大小
	ok, got, err := d.probe(context.Background(), d.key(p))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("s3 upload %s: object missing after upload", p)
	}
	if got != size {
		return fmt.Errorf("s3 upload %s: stored size %d != %d (body likely dropped by a proxy)", p, got, size)
	}
	return nil
}

// probe 用 list 精确探测单个对象：OpenList 不支持 HEAD（坑 5）
func (d *S3) probe(ctx context.Context, key string) (bool, int64, error) {
	out, err := d.cli.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(d.bucket),
		Prefix:  aws.String(key),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, 0, err
	}
	if len(out.Contents) == 0 || aws.ToString(out.Contents[0].Key) != key {
		return false, 0, nil
	}
	return true, aws.ToInt64(out.Contents[0].Size), nil
}

func (d *S3) IsExist(p string) (bool, error) {
	key := d.key(p)
	// OpenList/AList 不支持 HEAD（403），follow302 开启时用 list 探测
	if d.follow302 {
		ok, _, err := d.probe(context.Background(), key)
		return ok, err
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
	return d.DownloadMedia(context.Background(), p, -1, -1)
}

func (d *S3) DownloadWithOffset(p string, offset int64) (io.ReadCloser, int64, error) {
	return d.DownloadMedia(context.Background(), p, offset, -1)
}

// download：所有 GET 共享并发上限；body Close 时释放槽位。
func (d *S3) download(ctx context.Context, p string, offset int64) (io.ReadCloser, int64, error) {
	return d.DownloadMedia(ctx, p, offset, -1)
}

func (d *S3) DownloadMedia(ctx context.Context, p string, offset, end int64) (io.ReadCloser, int64, error) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	const stall = 30 * time.Second
	timer := time.AfterFunc(stall, cancel)
	var rc io.ReadCloser
	var size int64
	var err error
	if end >= 0 {
		rc, size, err = d.downloadBounded(ctx, p, offset, end)
	} else {
		rc, size, err = d.downloadInner(ctx, p, offset)
	}
	if err != nil {
		timer.Stop()
		cancel()
		<-d.sem
		return nil, 0, err
	}
	return &semBody{rc: rc, sem: d.sem, timer: timer, stall: stall, cancel: cancel}, size, nil
}

// semBody 在 Close 时释放并发槽位；sync.Once 防双释放。
type semBody struct {
	rc     io.ReadCloser
	sem    chan struct{}
	once   sync.Once
	timer  *time.Timer
	stall  time.Duration
	cancel context.CancelFunc
}

func (b *semBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.timer != nil {
		b.timer.Reset(b.stall)
	}
	return n, err
}

func (b *semBody) Close() error {
	var err error
	b.once.Do(func() {
		if b.timer != nil {
			b.timer.Stop()
			b.cancel()
		}
		err = b.rc.Close()
		<-b.sem
	})
	return err
}

// downloadInner 为原 download 主体。
func (d *S3) downloadInner(ctx context.Context, p string, offset int64) (io.ReadCloser, int64, error) {
	if offset < 0 {
		// 全量下载走分片：首片 Range bytes=0-2M 的 206 Content-Range 给出总大小，
		// 据此决定单流（小对象/服务端忽略 Range）或并行分片
		if d.follow302 {
			return d.downloadDirectFull(ctx, p)
		}
		return d.downloadFull(ctx, p)
	}
	if d.follow302 {
		// 标准 S3/R2/MinIO 支持签名 Range，先走 SDK 只取尾部；
		// OpenList 拒绝带 Range 的签名请求（坑 4），失败后退回 302 流程（本地丢弃前 offset 字节）
		if rc, size, err := d.getObject(ctx, p, offset); err == nil {
			return rc, size, nil
		}
		return d.downloadFollow302(ctx, p, offset)
	}
	return d.getObject(ctx, p, offset)
}

// downloadFull 走 SDK 的全量下载。
func (d *S3) downloadFull(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	first, err := d.getObjectRange(ctx, p, 0, firstChunkSize-1)
	if err != nil {
		if isRangeNotSatisfiable(err) {
			return d.getObject(ctx, p, -1) // 空对象：S3 对 Range 返回 416
		}
		return nil, 0, err
	}
	if first.ContentRange == nil {
		// 服务端忽略 Range，body 就是完整对象
		return first.Body, aws.ToInt64(first.ContentLength), nil
	}
	total, ok := parseTotal(aws.ToString(first.ContentRange))
	if !ok {
		first.Body.Close()
		return nil, 0, fmt.Errorf("s3 download %s: bad Content-Range %q", p, aws.ToString(first.ContentRange))
	}
	if total > parallelMaxSize {
		first.Body.Close()
		return d.getObject(ctx, p, -1) // 大文件退回单流
	}
	return stitchTails(ctx, first.Body, total, func(ctx context.Context, start, end int64) (io.ReadCloser, error) {
		out, err := d.getObjectRange(ctx, p, start, end)
		if err != nil {
			return nil, err
		}
		return out.Body, nil
	})
}

// downloadDirectFull 走 302 直链的全量下载：分片打在直链上（坑 4：签名请求不能带 Range）。
func (d *S3) downloadDirectFull(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	po, err := d.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
	})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, po.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	rsp, err := d.httpNoRedir.Do(req)
	if err != nil {
		return nil, 0, err
	}
	switch rsp.StatusCode {
	case http.StatusOK: // 服务端开了 web_proxy，直接中转
		return d.offsetBody(ctx, p, rsp, 0)
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
		return nil, 0, fmt.Errorf("s3 download %s: bad Location: %w", p, err)
	}
	if d.force302HTTPS && u.Scheme != "https" {
		return nil, 0, fmt.Errorf("s3 download %s: redirect Location scheme is %q, want https", p, u.Scheme)
	}

	first, err := d.directGet(ctx, loc, 0, firstChunkSize-1)
	if err != nil {
		return nil, 0, err
	}
	switch first.StatusCode {
	case http.StatusOK: // 直链忽略 Range，body 是完整对象
		return d.offsetBody(ctx, p, first, 0)
	case http.StatusRequestedRangeNotSatisfiable: // 空对象
		first.Body.Close()
		plain, err := d.directGet(ctx, loc, -1, -1)
		if err != nil {
			return nil, 0, err
		}
		return d.offsetBody(ctx, p, plain, 0)
	case http.StatusPartialContent:
	default:
		first.Body.Close()
		return nil, 0, fmt.Errorf("s3 direct download %s: unexpected status %d", p, first.StatusCode)
	}
	total, ok := parseTotal(first.Header.Get("Content-Range"))
	if !ok {
		first.Body.Close()
		return nil, 0, fmt.Errorf("s3 direct download %s: bad Content-Range %q", p, first.Header.Get("Content-Range"))
	}
	if total > parallelMaxSize {
		first.Body.Close()
		plain, err := d.directGet(ctx, loc, -1, -1) // 大文件退回单流
		if err != nil {
			return nil, 0, err
		}
		return d.offsetBody(ctx, p, plain, 0)
	}
	return stitchTails(ctx, first.Body, total, func(ctx context.Context, start, end int64) (io.ReadCloser, error) {
		r, err := d.directGet(ctx, loc, start, end)
		if err != nil {
			return nil, err
		}
		if r.StatusCode != http.StatusPartialContent {
			r.Body.Close()
			return nil, fmt.Errorf("s3 direct download %s: chunk %d-%d: status %d", p, start, end, r.StatusCode)
		}
		return r.Body, nil
	})
}

// directGet 请求直链；start<0 时不带 Range。
func (d *S3) directGet(ctx context.Context, loc string, start, end int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, err
	}
	if start >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}
	return d.httpFollow.Do(req)
}

func (d *S3) getObjectRange(ctx context.Context, p string, start, end int64) (*s3.GetObjectOutput, error) {
	return d.cli.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	})
}

func isRangeNotSatisfiable(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "InvalidRange" {
		return true
	}
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable
}

// tailRanges 计算首片之后的取片区间：拆成 ≤maxTails 片（调用方保证 total ≤ parallelMaxSize）。
func tailRanges(total int64) [][2]int64 {
	rem := total - firstChunkSize
	if rem <= 0 {
		return nil
	}
	n := (rem + tailFloor - 1) / tailFloor
	if n > maxTails {
		n = maxTails
	}
	per := rem / int64(n)
	ranges := make([][2]int64, 0, n)
	start := int64(firstChunkSize)
	for i := int64(0); i < n; i++ {
		end := start + per - 1
		if i == n-1 {
			end = total - 1
		}
		ranges = append(ranges, [2]int64{start, end})
		start = end + 1
	}
	return ranges
}

type tailResult struct {
	buf []byte
	err error
}

type tail struct {
	res chan tailResult
}

func startTail(ctx context.Context, fetch func(ctx context.Context, start, end int64) (io.ReadCloser, error), start, end int64) *tail {
	t := &tail{res: make(chan tailResult, 1)}
	go func() {
		rc, err := fetch(ctx, start, end)
		if err != nil {
			t.res <- tailResult{err: err}
			return
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.res <- tailResult{err: err}
			return
		}
		if int64(len(b)) != end-start+1 {
			t.res <- tailResult{err: fmt.Errorf("s3 download: chunk %d-%d got %d bytes", start, end, len(b))}
			return
		}
		t.res <- tailResult{buf: b}
	}()
	return t
}

// stitchTails 返回 first 与尾片按序拼接的 reader。
// 尾片立即并行启动（多连接绕开 TCP 慢启动），读到时通常已就绪；
// Close 取消未完成的请求，丢弃式读取（如 GCM header 嗅探）不浪费整文件下载。
func stitchTails(parent context.Context, first io.ReadCloser, total int64, fetch func(ctx context.Context, start, end int64) (io.ReadCloser, error)) (io.ReadCloser, int64, error) {
	ranges := tailRanges(total)
	if len(ranges) == 0 {
		return first, total, nil
	}
	ctx, cancel := context.WithCancel(parent)
	pd := &parallelDownload{first: first, cancel: cancel}
	for _, rg := range ranges {
		pd.tails = append(pd.tails, startTail(ctx, fetch, rg[0], rg[1]))
	}
	return pd, total, nil
}

// parallelDownload 按序读 first 再按序读尾片，实现 io.ReadCloser
type parallelDownload struct {
	first     io.ReadCloser
	tails     []*tail
	cancel    context.CancelFunc
	cur       io.Reader
	firstDone bool
	i         int
}

func (p *parallelDownload) next() (io.Reader, error) {
	if !p.firstDone {
		p.firstDone = true
		return p.first, nil
	}
	if p.i < len(p.tails) {
		r := <-p.tails[p.i].res
		p.i++
		if r.err != nil {
			return nil, r.err
		}
		return bytes.NewReader(r.buf), nil
	}
	return nil, nil
}

func (p *parallelDownload) Read(b []byte) (int, error) {
	for {
		if p.cur == nil {
			r, err := p.next()
			if err != nil {
				return 0, err
			}
			if r == nil {
				return 0, io.EOF
			}
			p.cur = r
		}
		n, err := p.cur.Read(b)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			p.cur = nil
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (p *parallelDownload) Close() error {
	p.cancel()
	return p.first.Close()
}

func (d *S3) downloadBounded(ctx context.Context, p string, offset, end int64) (io.ReadCloser, int64, error) {
	out, err := d.getObjectRange(ctx, p, offset, end)
	if err != nil {
		if d.follow302 && ctx.Err() == nil {
			return d.downloadFollow302(ctx, p, offset, end)
		}
		return nil, 0, err
	}
	if out.ContentRange == nil && offset > 0 {
		if _, err := io.CopyN(io.Discard, out.Body, offset); err != nil {
			out.Body.Close()
			return nil, 0, err
		}
	}
	if total, ok := parseTotal(aws.ToString(out.ContentRange)); ok {
		return out.Body, total, nil
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func (d *S3) getObject(ctx context.Context, p string, offset int64) (io.ReadCloser, int64, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
	}
	if offset >= 0 {
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
func (d *S3) downloadFollow302(ctx context.Context, p string, offset int64, ends ...int64) (io.ReadCloser, int64, error) {
	po, err := d.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.key(p)),
	})
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, po.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	rsp, err := d.httpNoRedir.Do(req)
	if err != nil {
		return nil, 0, err
	}

	switch rsp.StatusCode {
	case http.StatusOK: // 服务端开了 web_proxy，直接中转
		return d.offsetBody(ctx, p, rsp, offset)
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
		return nil, 0, fmt.Errorf("s3 download %s: bad Location: %w", p, err)
	}
	if d.force302HTTPS && u.Scheme != "https" {
		return nil, 0, fmt.Errorf("s3 download %s: redirect Location scheme is %q, want https", p, u.Scheme)
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, 0, err
	}
	if len(ends) > 0 {
		req2.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, ends[0]))
	} else if offset >= 0 {
		req2.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	// 直链是 CDN，可能再跳一次；不带任何签名头，跟随便安全
	rsp2, err := d.httpFollow.Do(req2)
	if err != nil {
		return nil, 0, err
	}
	if rsp2.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		rsp2.Body.Close()
		total, _ := parseTotal(rsp2.Header.Get("Content-Range"))
		if total == 0 {
			total = offset
		}
		return io.NopCloser(strings.NewReader("")), total, nil
	}
	if rsp2.StatusCode != http.StatusOK && rsp2.StatusCode != http.StatusPartialContent {
		rsp2.Body.Close()
		return nil, 0, fmt.Errorf("s3 direct download %s: unexpected status %d", p, rsp2.StatusCode)
	}
	return d.offsetBody(ctx, p, rsp2, offset)
}

// offsetBody 返回响应流与总大小；服务端忽略 Range 时本地丢弃前 offset 字节
func (d *S3) offsetBody(ctx context.Context, p string, rsp *http.Response, offset int64) (io.ReadCloser, int64, error) {
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
		// OpenList 开 web_proxy 后中转响应是分块的、无 Content-Length（上游会把 size 直接写进响应头，-1 会毁掉响应）；
		// 它的 list 仍可用，用 list 探测真实大小补齐
		if ok, size, err := d.probe(ctx, d.key(p)); err == nil && ok {
			return rsp.Body, size, nil
		}
		// shortcut: list 也不可用时（如无 list 的直链 CDN）仍报错，等遇到这类服务端再改成流式透传
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
	defer d.invalidateAlbumIndex()
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
	return d.RangeContext(context.Background(), dir, deal)
}

func (d *S3) RangeContext(ctx context.Context, dir string, deal func(fs.FileInfo) bool) error {
	prefix := d.prefix(dir)
	d.indexMu.Lock()
	if time.Now().Before(d.indexExpires) && !strings.HasPrefix(strings.TrimPrefix(prefix, d.prefix(".")), ".") {
		infos := d.index[prefix]
		d.indexMu.Unlock()
		dealAll(infos, deal)
		return nil
	}
	d.indexMu.Unlock()
	pg := s3.NewListObjectsV2Paginator(d.cli, &s3.ListObjectsV2Input{
		Bucket:    aws.String(d.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
		MaxKeys:   pageSize(),
	})
	var infos []fs.FileInfo
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
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
func (d *S3) invalidateAlbumIndex() {
	d.indexMu.Lock()
	defer d.indexMu.Unlock()
	d.index = nil
	d.indexExpires = time.Time{}
	d.indexVersion++
}

func (d *S3) PrepareAlbumIndex(ctx context.Context) error {
	var version uint64
	for {
		d.indexMu.Lock()
		if time.Now().Before(d.indexExpires) {
			d.indexMu.Unlock()
			return nil
		}
		wait := d.indexLoading
		if wait == nil {
			d.indexLoading = make(chan struct{})
			version = d.indexVersion
			d.indexMu.Unlock()
			break
		}
		d.indexMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() {
		d.indexMu.Lock()
		close(d.indexLoading)
		d.indexLoading = nil
		d.indexMu.Unlock()
	}()

	root := d.prefix(".")
	pages := s3.NewListObjectsV2Paginator(d.cli, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket), Prefix: aws.String(root), MaxKeys: pageSize(),
	})
	index := make(map[string][]fs.FileInfo)
	directories := make(map[string]bool)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if !strings.HasPrefix(key, root) || key == root {
				continue
			}
			relative := strings.TrimPrefix(key, root)
			parts := strings.Split(strings.TrimSuffix(relative, "/"), "/")
			if strings.HasPrefix(parts[0], ".") {
				continue
			}
			prefix := root
			for i, part := range parts {
				if i == len(parts)-1 && !strings.HasSuffix(key, "/") {
					index[prefix] = append(index[prefix], &fileInfo{
						name: part, size: aws.ToInt64(obj.Size), mod: aws.ToTime(obj.LastModified),
					})
					break
				}
				child := prefix + part + "/"
				if !directories[child] {
					directories[child] = true
					index[prefix] = append(index[prefix], &fileInfo{name: part, dir: true})
				}
				prefix = child
			}
		}
	}
	for _, infos := range index {
		sort.Sort(desc(infos))
	}
	d.indexMu.Lock()
	defer d.indexMu.Unlock()
	if d.indexVersion == version {
		d.index = index
		d.indexExpires = time.Now().Add(time.Minute)
	}
	return nil
}

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

func (f *fileInfo) Name() string { return f.name }
func (f *fileInfo) Size() int64  { return f.size }
func (f *fileInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (f *fileInfo) ModTime() time.Time { return f.mod }
func (f *fileInfo) IsDir() bool        { return f.dir }
func (f *fileInfo) Sys() any           { return nil }

type desc []fs.FileInfo

func (d desc) Len() int      { return len(d) }
func (d desc) Swap(i, j int) { d[i], d[j] = d[j], d[i] }
func (d desc) Less(i, j int) bool {
	if d[i].ModTime().Equal(d[j].ModTime()) {
		return d[i].Name() > d[j].Name()
	}
	return d[i].ModTime().After(d[j].ModTime())
}

func (d *S3) Close() error { return nil }
