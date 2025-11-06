package microvm

import "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"

var MicroSandboxSpecMap = map[string]v1alpha1.TemplateSpec{
	"code-interpreter": {
		TemplateId:          "wcriroew2qf81vxxc30f",
		BuildId:             "78771eac-140f-4024-a3e1-8e356d526962",
		BaseTemplateId:      "dbv8nep93eri7ecgfara",
		KernelVersion:       "vmlinux-6.1.102",
		FirecrackerVersion:  "v1.12.1_d990331",
		HugePages:           true,
		Vcpu:                2,
		RamMb:               1024,
		TotalDiskSizeMb:     5573,
		EnvdVersion:         "0.3.3",
		MaxSandboxLength:    1,
		AllowInternetAccess: true,
	},
	"desktop": {
		TemplateId:          "zj02gixajsggndugz36g",
		BuildId:             "cbbaf2e3-3630-4f43-8f74-c8fad47c5eca",
		BaseTemplateId:      "zj02gixajsggndugz36g",
		KernelVersion:       "vmlinux-6.1.102",
		FirecrackerVersion:  "v1.12.1_d990331",
		HugePages:           true,
		Vcpu:                8,
		RamMb:               8196,
		TotalDiskSizeMb:     5573,
		EnvdVersion:         "0.3.3",
		MaxSandboxLength:    1,
		AllowInternetAccess: true,
	},
}
