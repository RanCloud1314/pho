package smb

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hirochachacha/go-smb2"
)

const smbConnTTL = 5 * time.Minute

type Smb struct {
	addr              string
	username          string
	password          string
	shareName         string
	rootPath          string
	fs                *smb2.Share
	conn              net.Conn
	timeout           time.Duration
	lastConnTimestamp int64
	downloadLock      sync.Mutex
	connMu            sync.Mutex
}

func NewSmbDrive(addr, username, password string) *Smb {
	if strings.Index(addr, ":") < 0 {
		addr = addr + ":445"
	}
	smb := &Smb{
		addr:     addr,
		username: username,
		password: password,
	}

	return smb
}

// SetTimeoutSec 设置无进展超时秒数：传输中每次读写都会把 deadline 续到
// now+timeout，只有真正无进展超过该时长才会中断。0 或负数表示不限制。
func (s *Smb) SetTimeoutSec(sec int32) {
	if sec < 0 {
		sec = 0
	}
	s.timeout = time.Duration(sec) * time.Second
}

// deadlineReader 在每次 Read 前把 conn 的读写 deadline 续到 now+timeout。
// shortcut: 并发上传共享同一 conn 时 deadline 相互续期，单个卡死流检测不到；
// 需要精确按流超时时再升级为 per-stream 机制。
type deadlineReader struct {
	io.Reader
	s *Smb
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	if r.s.timeout > 0 && r.s.conn != nil {
		deadline := time.Now().Add(r.s.timeout)
		r.s.conn.SetReadDeadline(deadline)
		r.s.conn.SetWriteDeadline(deadline)
	}
	return r.Reader.Read(p)
}

func (r *deadlineReader) Close() error {
	r.s.clearDeadline()
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// clearDeadline 清除残留的读写 deadline，避免影响后续空闲期操作。
func (s *Smb) clearDeadline() {
	if s.conn != nil {
		var zero time.Time
		s.conn.SetDeadline(zero)
	}
}

// wrapStallTimeout 给 r 加上无进展超时；未启用（timeout=0）时原样返回。
func (s *Smb) wrapStallTimeout(r io.Reader) io.Reader {
	if s.timeout <= 0 {
		return r
	}
	return &deadlineReader{Reader: r, s: s}
}

func (s *Smb) lastConnTime() time.Time {
	ts := atomic.LoadInt64(&s.lastConnTimestamp)
	return time.Unix(ts, 0)
}

func (s *Smb) updateLastConnTime() {
	atomic.StoreInt64(&s.lastConnTimestamp, time.Now().Unix())
}

func (s *Smb) cleanLastConnTime() {
	atomic.StoreInt64(&s.lastConnTimestamp, 0)
}

func (s *Smb) checkConn() error {
	if time.Since(s.lastConnTime()) < smbConnTTL {
		if s.fs != nil {
			// 轻量健康检查：对已知存在的路径做 Stat
			checkPath := "/"
			if s.rootPath != "" {
				checkPath = s.rootPath
			}
			if _, err := s.fs.Stat(checkPath); err == nil {
				return nil
			}
			// Stat 失败，连接可能已断开，继续执行下方重连
		} else {
			return nil
		}
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	// 双重检查：获取锁后可能有其他 goroutine 已完成重连
	if time.Since(s.lastConnTime()) < smbConnTTL && s.fs != nil {
		if _, err := s.fs.Stat("/"); err == nil {
			return nil
		}
	}
	if s.fs != nil {
		_ = s.fs.Umount()
	}
	return s.Connect()
}

func (s *Smb) Dial() (*smb2.Session, error) {
	if s.addr == "" || s.username == "" {
		return nil, fmt.Errorf("smb config error: addr=%s, username=%s, password=***, shareName=%s", s.addr, s.username, s.shareName)
	}
	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).Dial("tcp", s.addr)
	if err != nil {
		return nil, err
	}
	s.conn = conn
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     s.username,
			Password: s.password,
		},
	}
	sess, err := d.Dial(conn)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Smb) Connect() error {
	if s.shareName == "" {
		return fmt.Errorf("smb share name is empty")
	}
	sess, err := s.Dial()
	if err != nil {
		return err
	}

	s.fs, err = sess.Mount(s.shareName)
	if err != nil {
		return err
	}
	s.updateLastConnTime()
	return nil
}

func (s *Smb) ListShare() ([]string, error) {
	sess, err := s.Dial()
	if err != nil {
		return nil, err
	}
	defer sess.Logoff()
	shares, err := sess.ListSharenames()
	if err != nil {
		return nil, err
	}
	return shares, nil
}

func (s *Smb) SetShare(shareName string) error {
	shares, err := s.ListShare()
	if err != nil {
		return err
	}
	for _, share := range shares {
		if share == shareName {
			s.shareName = shareName
			return nil
		}
	}
	return fmt.Errorf("share %s not exist", shareName)
}

func (s *Smb) IsShareSet() bool {
	return s.shareName != ""
}

func (s *Smb) IsRootPathSet() bool {
	return s.rootPath != ""
}

func (s *Smb) SetRootPath(rootPath string) error {
	if err := s.checkConn(); err != nil {
		return err
	}
	_, err := s.fs.Stat(rootPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("root path %s not exist", rootPath)
		}
		return err
	}
	s.rootPath = rootPath
	return nil
}

func (s *Smb) Upload(path string, content io.ReadCloser, size int64, lastModified time.Time) error {
	defer content.Close()
	if err := s.checkConn(); err != nil {
		return err
	}
	if s.rootPath == "" {
		return fmt.Errorf("root path is empty")
	}
	fullPath := filepath.Join(s.rootPath, path)
	if err := s.fs.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		s.cleanLastConnTime()
		return err
	}
	f, err := s.fs.Create(fullPath)
	if err != nil {
		s.cleanLastConnTime()
		return err
	}

	_, copyErr := io.Copy(f, s.wrapStallTimeout(content))
	s.clearDeadline()
	closeErr := f.Close()
	if copyErr != nil {
		// 删除残缺文件，防止 FilterNotUploaded 按名匹配将其标记为已上传（issue #83/#82）
		if rmErr := s.fs.Remove(fullPath); rmErr != nil {
			log.Printf("remove partial file failed (non-fatal): %v", rmErr)
		}
		return fmt.Errorf("copy error: %v", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close error: %v", closeErr)
	}
	if !lastModified.IsZero() {
		// 修改时间设置失败是非致命错误，仅记录日志
		if err := s.fs.Chtimes(fullPath, lastModified, lastModified); err != nil {
			log.Printf("set modification time failed (non-fatal): %v", err)
		}
	}
	s.updateLastConnTime()

	return nil
}

func (s *Smb) IsExist(path string) (bool, error) {
	if err := s.checkConn(); err != nil {
		return false, err
	}
	if s.rootPath == "" {
		return false, fmt.Errorf("root path is empty")
	}
	fullPath := filepath.Join(s.rootPath, path)
	_, err := s.fs.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.updateLastConnTime()
			return false, nil
		}
		s.cleanLastConnTime()
		return false, err
	}
	s.updateLastConnTime()
	return true, nil
}

func (s *Smb) Download(path string) (io.ReadCloser, int64, error) {
	// if err := s.checkConn(); err != nil {
	// 	return nil, 0, err
	// }
	// if s.rootPath == "" {
	// 	return nil, 0, fmt.Errorf("root path is empty")
	// }
	// fullPath := filepath.Join(s.rootPath, path)
	// f, err := s.fs.Open(fullPath)
	// if err != nil {
	// 	s.cleanLastConnTime()
	// 	return nil, 0, err
	// }
	// fi, err := f.Stat()
	// if err != nil {
	// 	return nil, 0, fmt.Errorf("stat %s error: %v", path, err)
	// }
	// log.Printf("got %s stat, size: %d", path, fi.Size())
	// s.updateLastConnTime()
	// return f, fi.Size(), nil
	return s.DownloadWithOffset(path, 0)
}

// DownloadWithOffset 从 SMB 远程文件读取指定偏移量起的内容。
// 注意：由于 go-smb2 库不支持在同一 Share 上并发读取，下载操作通过 downloadLock 串行化。
func (s *Smb) DownloadWithOffset(path string, offset int64) (io.ReadCloser, int64, error) {
	s.downloadLock.Lock()
	defer s.downloadLock.Unlock()
	if err := s.checkConn(); err != nil {
		return nil, 0, err
	}
	if s.rootPath == "" {
		return nil, 0, fmt.Errorf("root path is empty")
	}
	fullPath := filepath.Join(s.rootPath, path)
	f, err := s.fs.Open(fullPath)
	if err != nil {
		s.cleanLastConnTime()
		return nil, 0, err
	}
	s.updateLastConnTime()
	fi, err := f.Stat()
	if err != nil {
		s.cleanLastConnTime()
		return nil, 0, err
	}
	if offset > fi.Size() {
		return nil, 0, fmt.Errorf("offset %d is bigger than file size %d", offset, fi.Size())
	}
	_, err = f.Seek(offset, io.SeekStart)
	if err != nil {
		s.cleanLastConnTime()
		return nil, 0, err
	}
	// 包装无进展超时；Close 时会顺带清除 deadline
	if s.timeout > 0 {
		return &deadlineReader{Reader: f, s: s}, fi.Size(), nil
	}
	return f, fi.Size(), nil
}

func (s *Smb) Delete(path string) error {
	if err := s.checkConn(); err != nil {
		return err
	}
	if s.rootPath == "" {
		return fmt.Errorf("root path is empty")
	}
	fullPath := filepath.Join(s.rootPath, path)
	err := s.fs.Remove(fullPath)
	if err != nil {
		s.cleanLastConnTime()
		return err
	}
	s.updateLastConnTime()

	return nil
}

func (s *Smb) Close() error {
	if s.fs != nil {
		_ = s.fs.Umount()
	}
	return nil
}

func (s *Smb) Range(dir string, deal func(fs.FileInfo) bool) error {
	if err := s.checkConn(); err != nil {
		return err
	}
	if s.rootPath == "" {
		return fmt.Errorf("root path is empty")
	}
	fullPath := filepath.Join(s.rootPath, dir)
	infos, err := s.fs.ReadDir(fullPath)
	if err != nil {
		s.cleanLastConnTime()
		return err
	}
	s.updateLastConnTime()
	sort.Sort(desc(infos))
	for _, info := range infos {
		if !deal(info) {
			break
		}
	}
	return nil
}

type desc []fs.FileInfo

func (d desc) Len() int      { return len(d) }
func (d desc) Swap(i, j int) { d[i], d[j] = d[j], d[i] }
func (d desc) Less(i, j int) bool {
	return d[i].ModTime().After(d[j].ModTime())
}
