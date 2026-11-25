package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	pb "github.com/fregie/img_syncer/proto"
	"github.com/fregie/img_syncer/test/static"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
)

// 需要外部 S3 兼容服务端；未设置 S3_TEST_ENDPOINT 时跳过
type DriveS3TestSuite struct {
	suite.Suite
	srv      pb.ImgSyncerClient
	endpoint string
	bucket   string
	ak, sk   string
	region   string
}

func TestDriveS3TestSuite(t *testing.T) {
	suite.Run(t, new(DriveS3TestSuite))
}

func (s *DriveS3TestSuite) SetupTest() {
	s.endpoint = os.Getenv("S3_TEST_ENDPOINT")
	if s.endpoint == "" {
		s.T().Skip("S3_TEST_ENDPOINT not set; skipping s3 api tests")
	}
	s.region = envOrTest("S3_TEST_REGION", "us-east-1")
	s.ak = envOrTest("S3_TEST_AK", "fregie")
	s.sk = envOrTest("S3_TEST_SK", "password123")
	s.bucket = envOrTest("S3_TEST_BUCKET", "pho-api-test")

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(s.region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s.ak, s.sk, "")),
		config.WithBaseEndpoint(s.endpoint))
	s.Require().NoError(err)
	cli := awss3.NewFromConfig(cfg, func(o *awss3.Options) { o.UsePathStyle = true })

	_, err = cli.CreateBucket(context.Background(), &awss3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		s.Require().Contains(err.Error(), "BucketAlready", "create bucket: %v", err)
	}
	s.cleanBucket(cli)

	conn, err := grpc.Dial(grpcAddr, grpc.WithInsecure())
	s.Require().NoError(err)
	s.srv = pb.NewImgSyncerClient(conn)
}

func envOrTest(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *DriveS3TestSuite) cleanBucket(cli *awss3.Client) {
	out, err := cli.ListObjectsV2(context.Background(), &awss3.ListObjectsV2Input{Bucket: aws.String(s.bucket)})
	s.Require().NoError(err)
	for _, obj := range out.Contents {
		_, err := cli.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
			Bucket: aws.String(s.bucket), Key: obj.Key,
		})
		s.Require().NoError(err)
	}
}

func (s *DriveS3TestSuite) setDrive(root string) *pb.SetDriveS3Response {
	rsp, err := s.srv.SetDriveS3(context.Background(), &pb.SetDriveS3Request{
		Endpoint:        s.endpoint,
		Region:          s.region,
		AccessKeyId:     s.ak,
		SecretAccessKey: s.sk,
		Bucket:          s.bucket,
		Root:            root,
		PathStyle:       true,
		Follow_302:      false,
	})
	s.Require().NoError(err)
	return rsp
}

func (s *DriveS3TestSuite) TestSetDriveS3UploadDownload() {
	rsp := s.setDrive("photos")
	s.Truef(rsp.Success, "set drive failed: %s", rsp.Message)

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/pic1.jpg", httpAddr), bytes.NewReader(static.Pic1))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Image-Date", "2022:11:08 12:34:36")
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	s.Equal(http.StatusOK, resp.StatusCode)

	s.Require().NoError(waitfile(s.srv, pic1ShouldPath, 5*time.Second))

	// 下载校验
	dl, err := http.Get(fmt.Sprintf("http://%s/%s", httpAddr, pic1ShouldPath))
	s.Require().NoError(err)
	defer dl.Body.Close()
	s.Equal(http.StatusOK, dl.StatusCode)
	data, err := io.ReadAll(dl.Body)
	s.Require().NoError(err)
	s.Equal(static.Pic1, data)

	// 远端对象带 root 前缀
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(s.region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s.ak, s.sk, "")),
		config.WithBaseEndpoint(s.endpoint))
	s.Require().NoError(err)
	cli := awss3.NewFromConfig(cfg, func(o *awss3.Options) { o.UsePathStyle = true })
	_, err = cli.HeadObject(context.Background(), &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String("photos/" + pic1ShouldPath),
	})
	s.Require().NoError(err)
}

func (s *DriveS3TestSuite) TestFilterNotUploaded() {
	rsp := s.setDrive("")
	s.Truef(rsp.Success, "set drive failed: %s", rsp.Message)

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/pic1.jpg", httpAddr), bytes.NewReader(static.Pic1))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Image-Date", "2022:11:08 12:34:36")
	_, err = http.DefaultClient.Do(req)
	s.Require().NoError(err)
	s.Require().NoError(waitfile(s.srv, pic1ShouldPath, 5*time.Second))

	stream, err := s.srv.FilterNotUploaded(context.Background())
	s.Require().NoError(err)
	s.Require().NoError(stream.Send(&pb.FilterNotUploadedRequest{
		Photos: []*pb.FilterNotUploadedRequestInfo{
			{Name: "pic1.jpg", Date: "2022:11:08 12:34:36", Id: "id1"},
			{Name: "pic2.jpg", Date: "2022:11:08 12:34:36", Id: "id2"},
		},
		IsFinished: true,
	}))
	fsr, err := stream.Recv()
	s.Require().NoError(err)
	s.True(fsr.Success)
	s.Contains(fsr.UploadedIDs, "id1")
	s.Contains(fsr.NotUploadedIDs, "id2")
}

func (s *DriveS3TestSuite) TestSetDriveS3MissingField() {
	rsp, err := s.srv.SetDriveS3(context.Background(), &pb.SetDriveS3Request{
		Endpoint:        s.endpoint,
		Region:          s.region,
		AccessKeyId:     s.ak,
		SecretAccessKey: s.sk,
		Bucket:          "",
	})
	s.Require().NoError(err)
	s.False(rsp.Success)
	s.Contains(rsp.Message, "param error")
}
