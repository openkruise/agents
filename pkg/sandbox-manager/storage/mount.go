package storage

import (
	"fmt"

	"github.com/openkruise/agents/pkg/sandbox-manager/storage/alibabacloud"
)

const (
	ParamKeyMountPath     = "mountPath"
	ParamKeyVendor        = "vendor"
	VendorAlibabacloudNAS = "alibabacloud.com/nas"
	VendorAlibabacloudOSS = "alibabacloud.com/oss"
)

type MountOptions struct {
	MountPath string
	Args      vendorArgs
}

type vendorArgs interface {
	ToMountArgs() []string
}

func NewMountOptions(params map[string]string) (MountOptions, error) {
	path := params[ParamKeyMountPath]
	if path == "" {
		return MountOptions{}, fmt.Errorf("mount path is required")
	}
	args, err := newMountVendorArgs(params)
	if err != nil {
		return MountOptions{}, err
	}
	return MountOptions{
		MountPath: path,
		Args:      args,
	}, nil
}

func newMountVendorArgs(params map[string]string) (vendorArgs, error) {
	vendor := params[ParamKeyVendor]
	switch vendor {
	case VendorAlibabacloudNAS:
		return alibabacloud.NewMountVendorNAS(params)
	case VendorAlibabacloudOSS:
		return alibabacloud.NewMountVendorOSS(params)
	default:
		return nil, fmt.Errorf("unknown vendor: %s", vendor)
	}
}
