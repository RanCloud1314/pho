package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"
)

type uploadProgressKey struct{}

type UploadSnapshot struct {
	Phase     string  `json:"phase"`
	Confirmed int64   `json:"confirmed"`
	Total     int64   `json:"total"`
	Rate      float64 `json:"rate"`
}

type UploadProgress struct {
	mu        sync.Mutex
	state     UploadSnapshot
	wireBytes int64
	sampled   int64
	sampledAt time.Time
	parts     map[int]bool
	sentParts map[int]int64
}

func TrackUpload(ctx context.Context) (context.Context, *UploadProgress) {
	p := &UploadProgress{state: UploadSnapshot{Phase: "uploading"}, sampledAt: time.Now()}
	return context.WithValue(ctx, uploadProgressKey{}, p), p
}

func (p *UploadProgress) Snapshot() UploadSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	state := p.state
	if elapsed := now.Sub(p.sampledAt).Seconds(); elapsed > 0 {
		state.Rate = float64(p.wireBytes-p.sampled) / elapsed
		p.sampled, p.sampledAt = p.wireBytes, now
	}
	if state.Phase != "uploading" {
		state.Rate = 0
	}
	return state
}

type uploadHTTPClient struct {
	client interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func (c uploadHTTPClient) Do(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()
	part, _ := strconv.Atoi(q.Get("partNumber"))
	uploadID := q.Get("uploadId")
	var operation string
	switch {
	case req.Method == http.MethodPut && part > 0 && uploadID != "":
		operation = "UploadPart"
	case req.Method == http.MethodPut:
		operation = "PutObject"
	case req.Method == http.MethodPost && uploadID != "":
		operation = "CompleteMultipartUpload"
	case req.Method == http.MethodPost && q.Has("uploads"):
		operation = "CreateMultipartUpload"
	default:
		return c.client.Do(req)
	}

	p, _ := req.Context().Value(uploadProgressKey{}).(*UploadProgress)
	var payload int64
	if p != nil {
		p.mu.Lock()
		if operation == "CompleteMultipartUpload" {
			p.state.Phase = "confirming"
		} else if req.Method == http.MethodPut {
			payload = req.ContentLength
			if length, err := strconv.ParseInt(req.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64); err == nil {
				payload = length
			}
			p.state.Phase = "uploading"
			delete(p.sentParts, part)
			if req.Body != nil {
				body := &uploadBody{ReadCloser: req.Body, progress: p, length: req.ContentLength, part: part, payload: payload}
				req = req.Clone(req.Context())
				req.Body = body
			}
		}
		p.mu.Unlock()
	}
	resp, err := c.client.Do(req)
	if resp != nil {
		if part > 0 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			host := req.URL.Host
			if resp.Request != nil {
				host = resp.Request.URL.Host
			}
			log.Printf("S3 upload operation=%s status=%d host=%q content_type=%q request_id=%q upload_id=%q part=%d",
				operation, resp.StatusCode, host, resp.Header.Get("Content-Type"), resp.Header.Get("X-Amz-Request-Id"), uploadID, part)
		}
		if p != nil && err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.mu.Lock()
			if part > 0 && payload > 0 && !p.parts[part] {
				p.parts[part] = true
				p.state.Confirmed += payload
			}
			if operation == "PutObject" {
				p.state.Phase = "confirming"
			}
			p.mu.Unlock()
		}
	}
	return resp, err
}

type uploadBody struct {
	io.ReadCloser
	progress *UploadProgress
	length   int64
	read     int64
	part     int
	payload  int64
}

func (r *uploadBody) Read(buf []byte) (int, error) {
	n, err := r.ReadCloser.Read(buf)
	p := r.progress
	p.mu.Lock()
	defer p.mu.Unlock()
	r.read += int64(n)
	p.wireBytes += int64(n)
	if err == io.EOF || r.length >= 0 && r.read >= r.length {
		if r.part == 0 {
			p.state.Phase = "confirming"
		} else if r.payload > 0 {
			p.sentParts[r.part] = r.payload
			var sent int64
			for _, size := range p.sentParts {
				sent += size
			}
			if p.state.Total > 0 && sent >= p.state.Total {
				p.state.Phase = "confirming"
			}
		}
	}
	return n, err
}

func describeUploadError(err error) error {
	var op *smithy.OperationError
	var response *awshttp.ResponseError
	if !errors.As(err, &op) || !errors.As(err, &response) {
		return err
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return fmt.Errorf("S3 %s HTTP %d (%q)", op.OperationName, response.HTTPStatusCode(), apiError.ErrorCode())
	}
	return fmt.Errorf("S3 %s HTTP %d", op.OperationName, response.HTTPStatusCode())
}
