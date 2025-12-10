package client

import (
	"k8s.io/client-go/rest"
)

var (
	defaultGenericClient *GenericClientset
)

// NewRegistry creates clientset by client-go
func NewRegistry(c *rest.Config) error {
	var err error
	defaultGenericClient, err = newForConfig(c)
	if err != nil {
		return err
	}
	return nil
}

// GetGenericClient returns default clientset
func GetGenericClient() *GenericClientset {
	return defaultGenericClient
}
