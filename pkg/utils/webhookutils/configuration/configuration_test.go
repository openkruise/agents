/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package configuration

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

func TestGetPath(t *testing.T) {
	path := "/validate-sandbox"
	webhookURL := "https://webhook.example.com/validate-sandbox?source=test"
	invalidURL := "://invalid"

	tests := []struct {
		name         string
		clientConfig *admissionregistrationv1.WebhookClientConfig
		want         string
		wantErr      bool
	}{
		{
			name: "service path",
			clientConfig: &admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{Path: &path},
			},
			want: path,
		},
		{
			name: "service without path uses root",
			clientConfig: &admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{},
			},
			want: "/",
		},
		{
			name:         "URL path",
			clientConfig: &admissionregistrationv1.WebhookClientConfig{URL: &webhookURL},
			want:         path,
		},
		{
			name:         "invalid URL",
			clientConfig: &admissionregistrationv1.WebhookClientConfig{URL: &invalidURL},
			wantErr:      true,
		},
		{
			name:         "missing service and URL",
			clientConfig: &admissionregistrationv1.WebhookClientConfig{},
			wantErr:      true,
		},
		{
			name:    "nil client config",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getPath(tt.clientConfig)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("getPath() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("getPath() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("getPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertClientConfig(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		path string
		want string
	}{
		{
			name: "configured path",
			host: "webhook.example.com",
			port: 9443,
			path: "/validate-sandbox",
			want: "https://webhook.example.com:9443/validate-sandbox",
		},
		{
			name: "default root path",
			host: "webhook.example.com",
			port: 9443,
			path: "/",
			want: "https://webhook.example.com:9443/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConfig := &admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{},
			}

			convertClientConfig(clientConfig, tt.host, tt.port, tt.path)

			if clientConfig.Service != nil {
				t.Error("convertClientConfig() did not clear Service")
			}
			if clientConfig.URL == nil {
				t.Fatal("convertClientConfig() URL = nil")
			}
			if *clientConfig.URL != tt.want {
				t.Errorf("convertClientConfig() URL = %q, want %q", *clientConfig.URL, tt.want)
			}
		})
	}
}
