package utils

import (
	"flag"
	"fmt"
	"sync"

	consts2 "github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"k8s.io/klog/v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var initOnce sync.Once

func InitLogOutput() {
	initOnce.Do(func() {
		logf.SetLogger(klog.NewKlogr())
		klog.InitFlags(nil)
		_ = flag.Set("v", fmt.Sprintf("%d", consts2.DebugLogLevel))
		flag.Parse()
	})
}
