package imgmanager

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/fregie/img_syncer/proto"
	"github.com/fregie/img_syncer/server/util"
)

const (
	defaultWorkerNum          = 2
	defaultThumbnailMaxWidth  = 500
	defaultThumbnailMaxHeight = 500
	defaultThumbnailDir       = ".thumbnail"
)

type ImgManager struct {
	dri       StorageDrive
	driveMu   sync.RWMutex
	dirType   pb.DirectoryType
	dirTypeMu sync.RWMutex
	actCh     chan action
	stopCh    chan struct{}
	wg        sync.WaitGroup
	logger    *log.Logger
	opt       Option
}

type Option struct {
	WorkerNum          int
	ThumbnailMaxWidth  int
	ThumbnailMaxHeight int
	ThumbbailQuality   int
}

func NewImgManager(opt Option) *ImgManager {
	if opt.WorkerNum <= 0 {
		opt.WorkerNum = defaultWorkerNum
	}
	if opt.ThumbnailMaxWidth <= 0 {
		opt.ThumbnailMaxWidth = defaultThumbnailMaxWidth
	}
	if opt.ThumbnailMaxHeight <= 0 {
		opt.ThumbnailMaxHeight = defaultThumbnailMaxHeight
	}
	im := &ImgManager{
		actCh:  make(chan action, 10),
		stopCh: make(chan struct{}),
		logger: log.New(os.Stdout, "[ImgManager] ", log.LstdFlags),
		opt:    opt,
		dri:    &UnimplementedDrive{},
	}
	im.wg.Add(im.opt.WorkerNum)
	for i := 0; i < im.opt.WorkerNum; i++ {
		go im.runWorker()
	}
	return im
}

func (im *ImgManager) SetDirectoryType(dirType pb.DirectoryType) {
	im.dirTypeMu.Lock()
	defer im.dirTypeMu.Unlock()
	im.dirType = dirType
}

func (im *ImgManager) SetDrive(dri StorageDrive) {
	im.driveMu.Lock()
	defer im.driveMu.Unlock()
	if im.dri != nil {
		im.dri.Close()
	}
	im.dri = dri
}

func (im *ImgManager) Drive() StorageDrive {
	im.driveMu.RLock()
	defer im.driveMu.RUnlock()
	return im.dri
}

// Close 优雅关闭：关闭 stopCh 通知所有 worker 退出，等待 worker 完成后返回
func (im *ImgManager) Close() error {
	close(im.stopCh)
	im.wg.Wait()
	return nil
}

func (im *ImgManager) drive() (StorageDrive, func()) {
	im.driveMu.RLock()
	return im.dri, func() { im.driveMu.RUnlock() }
}

type actType int

const (
	actDelete actType = iota
)

type action struct {
	t    actType
	path string
}

func (im *ImgManager) runWorker() {
	for {
		select {
		case <-im.stopCh:
			im.wg.Done()
			return
		case act := <-im.actCh:
			switch act.t {
			case actDelete:
				func() {
					d, unlock := im.drive()
					defer unlock()
					err := d.Delete(act.path)
					if err != nil {
						im.logger.Println("Error deleting image:", err)
					}
				}()
			}
		}
	}
}

type Options struct {
	EncyptOption EncryptOption
	IsLivePhoto  bool
	ctx          context.Context
	rangeEnd     *int64
}

// EncryptType 和常量已迁移至 encrypt.go

type EncryptOption struct {
	Type     EncryptType
	Password string
}

type OptionFunc func(*Options)

func WithEncrypt(opt EncryptOption) OptionFunc {
	return func(o *Options) {
		o.EncyptOption = opt
	}
}

func WithMediaRequest(ctx context.Context, end int64) OptionFunc {
	return func(o *Options) {
		o.ctx = ctx
		o.rangeEnd = &end
	}
}

func WithUploadRequest(ctx context.Context) OptionFunc {
	return func(o *Options) { o.ctx = ctx }
}

func (o Options) upload(d StorageDrive, path string, r io.ReadCloser, size int64, date time.Time) error {
	if drive, ok := d.(interface {
		UploadContext(context.Context, string, io.ReadCloser, int64, time.Time) error
	}); ok && o.ctx != nil {
		return drive.UploadContext(o.ctx, path, r, size, date)
	}
	return d.Upload(path, r, size, date)
}

func (o Options) end() int64 {
	if o.rangeEnd == nil {
		return -1
	}
	return *o.rangeEnd
}

func (o Options) download(d StorageDrive, path string, offset, end int64) (io.ReadCloser, int64, error) {
	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if reader, ok := d.(interface {
		DownloadMedia(context.Context, string, int64, int64) (io.ReadCloser, int64, error)
	}); ok {
		return reader.DownloadMedia(ctx, path, offset, end)
	}
	if offset < 0 {
		return d.Download(path)
	}
	return d.DownloadWithOffset(path, offset)
}

func IsLivePhoto(isLivePhoto bool) OptionFunc {
	return func(o *Options) {
		o.IsLivePhoto = isLivePhoto
	}
}

func (im *ImgManager) genPath(name string, date time.Time, options Options) string {
	if date.IsZero() {
		date = time.Now()
	}
	if date.Before(time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)) {
		date = time.Now()
	}
	im.dirTypeMu.RLock()
	dirType := im.dirType
	im.dirTypeMu.RUnlock()
	elems := []string{}
	switch dirType {
	case pb.DirectoryType_DIRECTORY_TYPE_01:
		elems = append(elems, date.Format("2006/01/02"))
	case pb.DirectoryType_DIRECTORY_TYPE_02:
		elems = append(elems, date.Format("20060102/"))
	default:
		elems = append(elems, date.Format("2006/01/02"))
	}
	if options.IsLivePhoto {
		elems = append(elems, fmt.Sprintf("live_%s", name[:len(name)-len(filepath.Ext(name))]))
	}
	elems = append(elems, name)
	path := filepath.Join(elems...)
	if options.EncyptOption.Password != "" {
		path = fixPath(path, options.EncyptOption.Type)
	}
	return path
}

// sizeCheckReader 校验 reader 实际产出的字节数与声明一致。
// 防止截断上传（reader 提前 EOF）被当作完整文件保存为损坏文件（issue #83/#82）。
// 实现 io.ReadCloser：Close 透传到底层 reader，保证 GCM 加密 pipe 能被正常关闭。
type sizeCheckReader struct {
	r    io.Reader
	n    int64
	want int64
}

func (s *sizeCheckReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += int64(n)
	if err == io.EOF && s.n != s.want {
		return n, fmt.Errorf("upload size mismatch: got %d bytes, declared %d", s.n, s.want)
	}
	return n, err
}

func (s *sizeCheckReader) Close() error {
	if c, ok := s.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (im *ImgManager) Upload(content io.Reader, contentSize int64, name string, date time.Time, opts ...OptionFunc) error {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	path := im.genPath(name, date, options)
	if content == nil {
		return fmt.Errorf("content is nil")
	}
	reader, err := EncryptedReaderWraper(io.NopCloser(content), options.EncyptOption)
	if err != nil {
		im.logger.Println("Error encrypting:", err)
		return fmt.Errorf("error encrypting: %w", err)
	}
	if options.EncyptOption.Type != None && options.EncyptOption.Password != "" {
		contentSize = EncryptedContentSize(contentSize, options.EncyptOption.Type)
	}
	d, unlock := im.drive()
	defer unlock()
	e := options.upload(d, path, &sizeCheckReader{r: reader, want: contentSize}, contentSize, date)
	if e != nil {
		im.logger.Println("Error uploading:", e)
		return fmt.Errorf("error uploading: %w", e)
	}
	return nil
}

func (im *ImgManager) UploadThumbnail(thumbnailContent io.Reader, thumbnailSize int64, name string, date time.Time, opts ...OptionFunc) error {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	if thumbnailContent == nil {
		return fmt.Errorf("thumbnail content is nil")
	}
	path := im.genPath(name, date, options)
	reader, err := EncryptedReaderWraper(io.NopCloser(thumbnailContent), options.EncyptOption)
	if err != nil {
		im.logger.Println("Error encrypting video:", err)
		return fmt.Errorf("error encrypting video: %w", err)
	}
	if options.EncyptOption.Type != None && options.EncyptOption.Password != "" {
		thumbnailSize = EncryptedContentSize(thumbnailSize, options.EncyptOption.Type)
	}
	d, unlock := im.drive()
	defer unlock()
	e := options.upload(d, filepath.Join(defaultThumbnailDir, path),
		&sizeCheckReader{r: reader, want: thumbnailSize}, thumbnailSize, date)
	if e != nil {
		im.logger.Printf("Error uploading %s: %s", path, e)
		return fmt.Errorf("error uploading %s thumbnail: %w", path, e)
	}
	return nil
}

func (im *ImgManager) UploadLiveVideo(content io.Reader, size int64, name string, date time.Time, opts ...OptionFunc) error {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	if content == nil {
		return fmt.Errorf("thumbnail content is nil")
	}
	path := im.genPath(name, date, options)
	reader, err := EncryptedReaderWraper(io.NopCloser(content), options.EncyptOption)
	if err != nil {
		im.logger.Println("Error encrypting video:", err)
		return fmt.Errorf("error encrypting video: %w", err)
	}
	if options.EncyptOption.Type != None && options.EncyptOption.Password != "" {
		size = EncryptedContentSize(size, options.EncyptOption.Type)
	}
	d, unlock := im.drive()
	defer unlock()
	e := options.upload(d, path, &sizeCheckReader{r: reader, want: size}, size, date)
	if e != nil {
		im.logger.Printf("Error uploading %s: %s", path, e)
		return fmt.Errorf("error uploading %s thumbnail: %w", path, e)
	}
	return nil
}

func (im *ImgManager) GetImg(path string, opts ...OptionFunc) (*Image, error) {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	img := &Image{}
	var err error
	var rc io.ReadCloser
	d, unlock := im.drive()
	defer unlock()
	rc, img.Size, err = options.download(d, path, -1, -1)
	if err != nil {
		return img, err
	}
	encType := getPathEncType(path)
	if encType != None {
		detectedType, restoredRc, err := DetectEncryptFormat(rc)
		if err != nil {
			return img, err
		}
		options.EncyptOption.Type = detectedType
		img.Content, err = DecryptedReaderWraper(restoredRc, options.EncyptOption)
		if err != nil {
			return img, err
		}
		img.Size = DecryptedContentSize(img.Size, detectedType)
	} else {
		img.Content = rc
	}
	img.Path = path
	if filepath.Ext(path) == ".aes" {
		img.ContentType = mime.TypeByExtension(filepath.Ext(strings.TrimSuffix(path, ".aes")))
	} else {
		img.ContentType = mime.TypeByExtension(filepath.Ext(path))
	}
	return img, nil
}

func (im *ImgManager) GetOffset(path string, offset int64, opts ...OptionFunc) (*Image, error) {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	if getPathEncType(path) != None {
		// 加密文件：先读 header 判断 GCM/CFB
		d, unlock := im.drive()
		rc, storedSize, err := options.download(d, path, 0, int64(gcmHeaderLen)-1)
		if err != nil {
			unlock()
			return nil, err
		}
		magic := make([]byte, gcmMagicLen)
		if _, err := io.ReadFull(rc, magic); err != nil {
			rc.Close()
			unlock()
			return nil, fmt.Errorf("failed to read magic bytes: %w", err)
		}
		if string(magic) == gcmMagic {
			// GCM 路径 — 支持任意 offset
			// 继续读剩余 header: salt(16B) + chunkSize(4B)
			headerRemain := make([]byte, gcmSaltLen+4)
			if _, err := io.ReadFull(rc, headerRemain); err != nil {
				rc.Close()
				unlock()
				return nil, fmt.Errorf("failed to read GCM header: %w", err)
			}
			rc.Close() // 释放第一次 Download
			unlock()   // 释放 drive 锁

			salt := make([]byte, gcmSaltLen)
			copy(salt, headerRemain[:gcmSaltLen])
			chunkSize := binary.BigEndian.Uint32(headerRemain[gcmSaltLen:])

			plainSize := DecryptedContentSize(storedSize, AES_256_GCM)
			if chunkSize == 0 || offset >= plainSize {
				return nil, fmt.Errorf("invalid encrypted media range")
			}
			chunkIndex := offset / int64(chunkSize)
			withinChunk := offset % int64(chunkSize)
			ciphertextStart := int64(gcmHeaderLen) + chunkIndex*(int64(chunkSize)+int64(gcmNonceSize)+int64(gcmTagSize))

			ciphertextEnd := int64(-1)
			if end := options.end(); end >= 0 {
				lastChunk := min(end, plainSize-1) / int64(chunkSize)
				ciphertextEnd = min(storedSize-1,
					int64(gcmHeaderLen)+(lastChunk+1)*int64(chunkSize+gcmNonceSize+gcmTagSize)-1)
			}
			d2, unlock2 := im.drive()
			diskRc, diskSize, err := options.download(d2, path, ciphertextStart, ciphertextEnd)
			if err != nil {
				unlock2()
				return nil, err
			}
			if diskSize != storedSize {
				diskRc.Close()
				unlock2()
				return nil, fmt.Errorf("stored size mismatch: first=%d second=%d", storedSize, diskSize)
			}

			seekRc, err := NewGcmSeekReader(diskRc, options.EncyptOption.Password, GcmSeekOpts{
				ChunkSize:         chunkSize,
				Salt:              salt,
				TotalStoredSize:   storedSize,
				StartChunkIndex:   chunkIndex,
				WithinChunkOffset: withinChunk,
			})
			if err != nil {
				unlock2()
				return nil, err
			}

			img := &Image{
				Content:     seekRc,
				Size:        DecryptedContentSize(storedSize, AES_256_GCM),
				Path:        path,
				ContentType: util.ContentTypeByExtension(filepath.Ext(strings.TrimSuffix(path, ".aes"))),
			}
			// 注意：seekRc.Close() 负责关闭 diskRc，但 drive 锁需要在此释放
			unlock2()
			return img, nil
		}
		// CFB 旧路径：不支持 seek，仅 offset=0 走 GetImg 兜底
		rc.Close()
		unlock()
		if offset == 0 {
			return im.GetImg(path, opts...)
		}
		return nil, fmt.Errorf("encrypted file (CFB) not support get offset")
	}
	img := &Image{}
	var err error
	d, unlock := im.drive()
	defer unlock()
	img.Content, img.Size, err = options.download(d, path, offset, options.end())
	if err != nil {
		return img, err
	}
	img.Path = path
	if filepath.Ext(path) == ".aes" {
		img.ContentType = mime.TypeByExtension(filepath.Ext(strings.TrimSuffix(path, ".aes")))
	} else {
		img.ContentType = mime.TypeByExtension(filepath.Ext(path))
	}
	return img, nil
}

func (im *ImgManager) GetThumbnail(path string, opts ...OptionFunc) (*Image, error) {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	img := &Image{}
	var err error
	var rc io.ReadCloser
	thumbnailPath := filepath.Join(defaultThumbnailDir, path)
	d, unlock := im.drive()
	defer unlock()
	rc, img.Size, err = options.download(d, thumbnailPath, -1, -1)
	if err != nil {
		return img, fmt.Errorf("error downloading thumbnail: %w", err)
	}
	encType := getPathEncType(path)
	if encType != None {
		detectedType, restoredRc, err := DetectEncryptFormat(rc)
		if err != nil {
			return img, fmt.Errorf("error detecting encrypt format: %w", err)
		}
		options.EncyptOption.Type = detectedType
		img.Content, err = DecryptedReaderWraper(restoredRc, options.EncyptOption)
		if err != nil {
			return img, fmt.Errorf("error decrypting thumbnail: %w", err)
		}
		img.Size = DecryptedContentSize(img.Size, detectedType)
	} else {
		img.Content = rc
	}
	img.Path = thumbnailPath
	if filepath.Ext(path) == ".aes" {
		img.ContentType = mime.TypeByExtension(filepath.Ext(strings.TrimSuffix(path, ".aes")))
	} else {
		img.ContentType = mime.TypeByExtension(filepath.Ext(thumbnailPath))
	}
	return img, nil
}

func (im *ImgManager) GetLiveVideoOffset(path string, offset int64, opts ...OptionFunc) (*Image, error) {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}
	var videoPath string
	dir := filepath.Dir(path)
	err := func() error {
		d, unlock := im.drive()
		defer unlock()
		return d.Range(dir, func(info fs.FileInfo) bool {
			name := info.Name()
			nameWithoutExt := strings.TrimSuffix(strings.TrimSuffix(name, ".aes"), filepath.Ext(strings.TrimSuffix(name, ".aes")))
			pathNameWithoutExt := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".aes"), filepath.Ext(strings.TrimSuffix(path, ".aes")))
			if util.IsVideo(name) && nameWithoutExt == pathNameWithoutExt {
				videoPath = filepath.Join(dir, name)
				return false
			}
			return true
		})
	}()
	if err != nil {
		return nil, err
	}
	if videoPath == "" {
		return nil, fmt.Errorf("video not found")
	}
	if getPathEncType(videoPath) != None {
		// 加密视频：先读 header 判断 GCM/CFB
		d, unlock := im.drive()
		rc, storedSize, err := options.download(d, videoPath, 0, int64(gcmHeaderLen)-1)
		if err != nil {
			unlock()
			return nil, err
		}
		magic := make([]byte, gcmMagicLen)
		if _, err := io.ReadFull(rc, magic); err != nil {
			rc.Close()
			unlock()
			return nil, fmt.Errorf("failed to read magic bytes: %w", err)
		}
		if string(magic) == gcmMagic {
			// GCM 路径 — 支持任意 offset
			headerRemain := make([]byte, gcmSaltLen+4)
			if _, err := io.ReadFull(rc, headerRemain); err != nil {
				rc.Close()
				unlock()
				return nil, fmt.Errorf("failed to read GCM header: %w", err)
			}
			rc.Close()
			unlock()

			salt := make([]byte, gcmSaltLen)
			copy(salt, headerRemain[:gcmSaltLen])
			chunkSize := binary.BigEndian.Uint32(headerRemain[gcmSaltLen:])

			plainSize := DecryptedContentSize(storedSize, AES_256_GCM)
			if chunkSize == 0 || offset >= plainSize {
				return nil, fmt.Errorf("invalid encrypted media range")
			}
			chunkIndex := offset / int64(chunkSize)
			withinChunk := offset % int64(chunkSize)
			ciphertextStart := int64(gcmHeaderLen) + chunkIndex*(int64(chunkSize)+int64(gcmNonceSize)+int64(gcmTagSize))

			ciphertextEnd := int64(-1)
			if end := options.end(); end >= 0 {
				lastChunk := min(end, plainSize-1) / int64(chunkSize)
				ciphertextEnd = min(storedSize-1,
					int64(gcmHeaderLen)+(lastChunk+1)*int64(chunkSize+gcmNonceSize+gcmTagSize)-1)
			}
			d2, unlock2 := im.drive()
			diskRc, diskSize, err := options.download(d2, videoPath, ciphertextStart, ciphertextEnd)
			if err != nil {
				unlock2()
				return nil, err
			}
			if diskSize != storedSize {
				diskRc.Close()
				unlock2()
				return nil, fmt.Errorf("stored size mismatch: first=%d second=%d", storedSize, diskSize)
			}

			seekRc, err := NewGcmSeekReader(diskRc, options.EncyptOption.Password, GcmSeekOpts{
				ChunkSize:         chunkSize,
				Salt:              salt,
				TotalStoredSize:   storedSize,
				StartChunkIndex:   chunkIndex,
				WithinChunkOffset: withinChunk,
			})
			if err != nil {
				unlock2()
				return nil, err
			}

			img := &Image{
				Content:     seekRc,
				Size:        DecryptedContentSize(storedSize, AES_256_GCM),
				Path:        videoPath,
				ContentType: util.ContentTypeByExtension(filepath.Ext(strings.TrimSuffix(videoPath, ".aes"))),
			}
			unlock2()
			return img, nil
		}
		// CFB 旧路径：不支持 seek，仅 offset=0 走 GetImg 兜底
		rc.Close()
		unlock()
		if offset == 0 {
			return im.GetImg(videoPath, opts...)
		}
		return nil, fmt.Errorf("encrypted file (CFB) not support get offset")
	}
	// log.Printf("load video: %s", videoPath)
	img := &Image{}
	d, unlock := im.drive()
	defer unlock()
	img.Content, img.Size, err = options.download(d, videoPath, offset, options.end())
	if err != nil {
		return img, err
	}
	img.Path = videoPath
	if filepath.Ext(videoPath) == ".aes" {
		img.ContentType = util.ContentTypeByExtension(filepath.Ext(strings.TrimSuffix(videoPath, ".aes")))
	} else {
		img.ContentType = util.ContentTypeByExtension(filepath.Ext(videoPath))
	}
	// log.Printf("Live video path: %s", videoPath)
	// log.Printf("Content-Type: %s", img.ContentType)
	return img, nil
}

func (im *ImgManager) DeleteSingleImg(path string) error {
	if path != "" {
		d, unlock := im.drive()
		defer unlock()
		err := d.Delete(filepath.Join(defaultThumbnailDir, path))
		if err != nil {
			im.logger.Println("Error deleting thumbnail:", err)
		}
		parentDir := filepath.Base(filepath.Dir(filepath.ToSlash(path)))
		if strings.HasPrefix(parentDir, "live_") {
			d.Range(filepath.Dir(path), func(info fs.FileInfo) bool {
				err := d.Delete(filepath.Join(filepath.Dir(path), info.Name()))
				if err != nil {
					im.logger.Println("Error deleting live video:", err)
				}
				return true
			})
			err := d.Delete(filepath.Dir(path))
			if err != nil {
				im.logger.Println("Error deleting live video dir:", err)
			}
		}
		return d.Delete(path)
	}
	return nil
}

func (im *ImgManager) DeleteSingleImgAsync(path string) {
	if path != "" {
		im.actCh <- action{t: actDelete, path: path}
	}
}

func (im *ImgManager) DeleteImg(paths []string) {
	for _, path := range paths {
		if path != "" {
			im.DeleteSingleImg(path)
		}
	}
}

type dirInfo struct {
	date   time.Time
	dir    string
	levels int
}

type ImgInfo struct {
	Date        time.Time
	Path        string
	Size        int64
	IsLivePhoto bool
}

func (im *ImgManager) RangeByDate(date time.Time, f func(info ImgInfo) bool) error {
	return im.RangeAlbum(context.Background(), date, false, f)
}

func (im *ImgManager) RangeAlbum(ctx context.Context, date time.Time, prepareIndex bool, f func(info ImgInfo) bool) error {
	d, unlock := im.drive()
	defer unlock()
	if date.IsZero() {
		date = time.Now()
	}
	location := date.Location()
	if prepareIndex {
		if index, ok := d.(interface{ PrepareAlbumIndex(context.Context) error }); ok {
			if err := index.PrepareAlbumIndex(ctx); err != nil {
				return err
			}
		}
	}
	rangeDir := d.Range
	if drive, ok := d.(interface {
		RangeContext(context.Context, string, func(fs.FileInfo) bool) error
	}); ok {
		rangeDir = func(dir string, deal func(fs.FileInfo) bool) error {
			return drive.RangeContext(ctx, dir, deal)
		}
	}
	root, err := listDir(rangeDir, ".")
	if err != nil {
		im.logger.Println("Error listing year dir:", err)
		return err
	}

	var pending []dirInfo
	for _, info := range root {
		if !info.IsDir() {
			continue
		}
		name := info.Name()
		switch len(name) {
		case 8:
			day, err := time.ParseInLocation("20060102", name, location)
			if err == nil && !day.After(date) {
				pending = append(pending, dirInfo{date: day, dir: name})
			}
		case 4:
			year, err := strconv.Atoi(name)
			if err == nil && year > 0 && year <= date.Year() {
				pending = append(pending, dirInfo{
					date: time.Date(year, time.December, 31, 0, 0, 0, 0, location),
					dir:  name, levels: 2,
				})
			}
		}
	}

	// 年、月先按其最晚可能的日期排序，只展开当前页需要访问的目录。
	sortPending := func() {
		sort.Slice(pending, func(i, j int) bool {
			if pending[i].date.Equal(pending[j].date) {
				if pending[i].levels != pending[j].levels {
					return pending[i].levels < pending[j].levels
				}
				return pending[i].dir > pending[j].dir
			}
			return pending[i].date.Before(pending[j].date)
		})
	}
	sortPending()
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if current.levels > 0 {
			children, err := listDir(rangeDir, current.dir)
			if err != nil {
				return fmt.Errorf("list %s: %w", current.dir, err)
			}
			for _, child := range children {
				number, err := strconv.Atoi(child.Name())
				if !child.IsDir() || err != nil || number < 1 {
					continue
				}
				var day time.Time
				year, month, lastDay := current.date.Date()
				if current.levels == 2 {
					if number > 12 {
						continue
					}
					day = time.Date(year, time.Month(number)+1, 0, 0, 0, 0, 0, location)
					if year == date.Year() && day.Month() > date.Month() {
						continue
					}
				} else {
					if number > lastDay {
						continue
					}
					day = time.Date(year, month, number, 0, 0, 0, 0, location)
					if day.After(date) {
						continue
					}
				}
				pending = append(pending, dirInfo{
					date: day, dir: filepath.Join(current.dir, child.Name()), levels: current.levels - 1,
				})
			}
			sortPending()
			continue
		}

		goOn := true
		var liveErr error
		err := rangeDir(current.dir, func(info fs.FileInfo) bool {
			path := filepath.Join(current.dir, info.Name())
			if info.IsDir() {
				if strings.HasPrefix(info.Name(), "live_") {
					liveErr = rangeDir(path, func(info2 fs.FileInfo) bool {
						if !util.IsVideo(info2.Name()) {
							goOn = f(ImgInfo{
								Date: current.date, Path: filepath.Join(path, info2.Name()),
								Size: info2.Size(), IsLivePhoto: true,
							})
						}
						return goOn
					})
				}
				return goOn && liveErr == nil
			}
			goOn = f(ImgInfo{Date: current.date, Path: path, Size: info.Size()})
			return goOn
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", current.dir, err)
		}
		if liveErr != nil {
			return liveErr
		}
		if !goOn {
			break
		}
	}
	return nil
}
func listDir(rangeDir func(string, func(fs.FileInfo) bool) error, path string) ([]fs.FileInfo, error) {
	infos := make([]fs.FileInfo, 0)
	err := rangeDir(path, func(info fs.FileInfo) bool {
		infos = append(infos, info)
		return true
	})
	return infos, err
}
