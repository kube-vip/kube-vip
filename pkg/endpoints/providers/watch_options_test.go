package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestRetryWatchersPreserveResumeOptions(t *testing.T) {
	tests := []struct {
		name          string
		provider      Provider
		wantPath      string
		selectorKey   string
		selectorValue string
		watchEvent    string
	}{
		{
			name:          "EndpointSlice",
			provider:      NewEndpointslices(),
			wantPath:      "/apis/discovery.k8s.io/v1/namespaces/test/endpointslices",
			selectorKey:   "labelSelector",
			selectorValue: "kubernetes.io/service-name=snf",
			watchEvent:    `{"type":"ADDED","object":{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice","metadata":{"name":"snf-test","namespace":"test","resourceVersion":"42"},"addressType":"IPv4","ports":[],"endpoints":[]}}`,
		},
		{
			name:          "Endpoints",
			provider:      NewEndpoints(),
			wantPath:      "/api/v1/namespaces/test/endpoints",
			selectorKey:   "fieldSelector",
			selectorValue: "metadata.name=snf",
			watchEvent:    `{"type":"ADDED","object":{"apiVersion":"v1","kind":"Endpoints","metadata":{"name":"snf","namespace":"test","resourceVersion":"42"},"subsets":[]}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestURLs := make(chan *url.URL, 2)
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requestURL := *request.URL
				requestURLs <- &requestURL
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				if requestCount.Add(1) == 1 {
					_, _ = fmt.Fprintln(writer, test.watchEvent)
				}
				if flusher, ok := writer.(http.Flusher); ok {
					flusher.Flush()
				}
				if requestCount.Load() > 1 {
					<-request.Context().Done()
				}
			}))
			defer server.Close()

			clientSet, err := kubernetes.NewForConfig(&rest.Config{
				Host: server.URL,
				ContentConfig: rest.ContentConfig{
					ContentType: "application/json",
				},
			})
			if err != nil {
				t.Fatalf("create Kubernetes client: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			watcher, err := test.provider.CreateRetryWatcher(ctx, clientSet, &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "snf", Namespace: "test"},
			})
			if err != nil {
				t.Fatalf("CreateRetryWatcher() error = %v", err)
			}
			defer watcher.Stop()

			firstRequestURL := receiveRequestURL(t, requestURLs)

			select {
			case event := <-watcher.ResultChan():
				if event.Object == nil {
					t.Fatal("first watch event has no object")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for the first watch event")
			}

			secondRequestURL := receiveRequestURL(t, requestURLs)
			assertWatchRequest(t, firstRequestURL, test.wantPath, test.selectorKey, test.selectorValue, "")
			assertWatchRequest(t, secondRequestURL, test.wantPath, test.selectorKey, test.selectorValue, "42")
		})
	}
}

func receiveRequestURL(t *testing.T, requestURLs <-chan *url.URL) *url.URL {
	t.Helper()
	select {
	case requestURL := <-requestURLs:
		return requestURL
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the watch request")
		return nil
	}
}

func assertWatchRequest(t *testing.T, requestURL *url.URL, wantPath, selectorKey, selectorValue,
	resourceVersion string) {
	t.Helper()
	if requestURL.Path != wantPath {
		t.Errorf("watch path = %q, want %q", requestURL.Path, wantPath)
	}
	query := requestURL.Query()
	if got := query.Get("resourceVersion"); got != resourceVersion {
		t.Errorf("resourceVersion = %q, want %q", got, resourceVersion)
	}
	if got := query.Get("allowWatchBookmarks"); got != "true" {
		t.Errorf("allowWatchBookmarks = %q, want true", got)
	}
	if got := query.Get(selectorKey); got != selectorValue {
		t.Errorf("%s = %q, want %q", selectorKey, got, selectorValue)
	}
}
