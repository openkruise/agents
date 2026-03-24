package utils

import (
	"flag"
	"fmt"
	"sync"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"k8s.io/klog/v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var initOnce sync.Once

// InitLogOutput will be renamed to InitTestEnv in the future and will be moved to test/utils
func InitLogOutput() {
	initOnce.Do(func() {
		logf.SetLogger(klog.NewKlogr())
		klog.InitFlags(nil)
		_ = flag.Set("v", fmt.Sprintf("%d", consts.DebugLogLevel))
		flag.Parse()
	})
}
