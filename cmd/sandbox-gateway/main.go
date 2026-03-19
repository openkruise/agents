package main

import (
	"context"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	envoyhttp "github.com/envoyproxy/envoy/contrib/golang/filters/http/source/go/pkg/http"

	"github.com/openkruise/agents/pkg/sandbox-gateway/controller"
	"github.com/openkruise/agents/pkg/sandbox-gateway/filter"
)

func init() {
	envoyhttp.RegisterHttpFilterFactoryAndConfigParser(
		"sandbox-gateway",
		filter.FilterFactory,
		&filter.ConfigParser{},
	)

	go func() {
		if err := controller.StartManager(context.Background()); err != nil {
			api.LogErrorf("sandbox controller manager exited with error: %v", err)
		}
	}()
}

func main() {}
