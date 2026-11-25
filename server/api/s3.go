package api

import (
	"context"
	"fmt"
	"time"

	pb "github.com/fregie/img_syncer/proto"
	"github.com/fregie/img_syncer/server/drive/s3"
)

func (a *api) SetDriveS3(ctx context.Context, req *pb.SetDriveS3Request) (rsp *pb.SetDriveS3Response, e error) {
	rsp = &pb.SetDriveS3Response{Success: true}
	if req.Region == "" || req.AccessKeyId == "" || req.SecretAccessKey == "" || req.Bucket == "" {
		rsp.Success, rsp.Message = false, "param error: region/access_key_id/secret_access_key/bucket is required"
		return
	}
	d, err := s3.NewS3(req.Endpoint, req.Region, req.AccessKeyId, req.SecretAccessKey,
		req.Bucket, req.Root, req.PathStyle, req.Follow_302, req.Force_302Https)
	if err != nil {
		rsp.Success, rsp.Message = false, fmt.Sprintf("create s3 client failed: %s", err.Error())
		return
	}
	// 探测上限 60s，失败不装进 ImgManager
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := d.Ping(pctx); err != nil {
		rsp.Success, rsp.Message = false, fmt.Sprintf("connect to bucket %s failed: %s", req.Bucket, err.Error())
		return
	}
	a.im.SetDrive(d)
	return
}
