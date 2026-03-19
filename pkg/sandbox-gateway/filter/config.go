package filter

import (
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"google.golang.org/protobuf/types/known/anypb"
)

type config struct{}

type ConfigParser struct{}

func (p *ConfigParser) Parse(any *anypb.Any, callbacks api.ConfigCallbackHandler) (interface{}, error) {
	return &config{}, nil
}

func (p *ConfigParser) Merge(parent interface{}, child interface{}) interface{} {
	return child
}
