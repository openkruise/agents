package alibabacloud

import (
	"fmt"
	"strings"
)

type OSS struct {
	AccessKey string
	SecretKey string
	Bucket    string
	Endpoint  string
}

func (m OSS) ToMountArgs() []string {
	return []string{
		"--accessKey", m.AccessKey,
		"--secretKey", m.SecretKey,
		"--bucketName", m.Bucket,
		"--endpoint", m.Endpoint,
	}
}

const (
	OSSParamEndpoint = "endpoint"
	OSSParamBucket   = "bucket"
	OSSParamAK       = "accessKey"
	OSSParamSK       = "secretKey"
)

func NewMountVendorOSS(params map[string]string) (OSS, error) {
	endpoint, bucket, ak, sk := params[OSSParamEndpoint], params[OSSParamBucket], params[OSSParamAK], params[OSSParamSK]
	if !validateOssEndpoint(endpoint) {
		return OSS{}, fmt.Errorf("invalid endpoint: %s", endpoint)
	}
	if bucket == "" {
		return OSS{}, fmt.Errorf("bucket name is empty")
	}
	if ak == "" || sk == "" {
		return OSS{}, fmt.Errorf("access key and secret key are required")
	}
	return OSS{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: ak,
		SecretKey: sk,
	}, nil
}

func validateOssEndpoint(endpoint string) bool {
	return strings.HasSuffix(endpoint, ".aliyuncs.com")
}
