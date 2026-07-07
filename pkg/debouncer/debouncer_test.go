package debouncer

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestTimeSetting(t *testing.T) {
	tcs := []struct {
		name       string
		configured string
		expected   string
	}{
		{
			name:       "configured proper value 10s",
			configured: "10s",
			expected:   "10s",
		},
		{
			name:       "configured value less than 200ms",
			configured: "0s",
			expected:   "disabled",
		},
		{
			name:       "configured proper value 1s",
			configured: "1s",
			expected:   "1s",
		},
		{
			name:       "configured proper value 1500ms",
			configured: "1500ms",
			expected:   "1.5s",
		},
		{
			name:       "configured to value greater than 0s but lower than 200ms",
			configured: "150ms",
			expected:   "disabled",
		},
		{
			name:       "configured invalid value that cannot be parsed",
			configured: "invalid",
			expected:   "error",
		},
		{
			name:       "configured negative value",
			configured: "-1s",
			expected:   "disabled",
		},
	}

	input := make(chan watch.Event)
	defer close(input)

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			d, err := New(input, tc.configured)

			switch tc.expected {
			case "disabled":
				if err != nil {
					t.Fatalf("failed to create debouncer with debounce time %q", tc.configured)
				}
				if d != nil {
					t.Fatalf("debouncer was created but should be disabled for value %q", tc.configured)
				}
			case "error":
				if err == nil {
					t.Fatalf("debouncer was created but should error for value %q", tc.configured)
				}
			default:
				if d == nil {
					t.Fatalf("debouncer was not created for value %q", tc.configured)
				}

				if d.debounceTime.String() != tc.expected {
					t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), tc.expected)
				}
			}

		})
	}
}

func TestStartStop(t *testing.T) {
	t.Run("Run and stop the debouncer without issues", func(t *testing.T) {
		input := make(chan watch.Event)
		defer close(input)

		expected := "200ms"

		d, err := New(input, expected)

		if err != nil {
			t.Fatalf("failed to create debouncer with debounce time %q", expected)
		}

		if d.debounceTime.String() != expected {
			t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), expected)
		}

		ctx, cancel := context.WithCancel(context.Background())

		wg := sync.WaitGroup{}

		wg.Go(func() {
			if err := d.Start(ctx); err != nil {
				t.Fatalf("debouncer error: %s", err.Error())
			}
		})

		cancel()

		timedOut := waitTimeout(&wg, time.Second*3)

		if timedOut {
			t.Fatal("debouncer was not closed before timeout")
		}
	})
}

func TestDebouncing(t *testing.T) {
	tcs := []string{"endpointslices", "endpoints", "services"}

	for _, tc := range tcs {
		t.Run(fmt.Sprintf("Get the newest event as the only one when using %s", tc), func(t *testing.T) {
			expected := "500ms"

			fw := watch.NewFake()
			defer fw.Stop()

			d, err := New(fw.ResultChan(), expected)

			if err != nil {
				t.Fatalf("failed to create debouncer with debounce time %q", expected)
			}

			if d.debounceTime.String() != expected {
				t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), expected)
			}

			ctx, cancel := context.WithCancel(context.Background())

			wg := sync.WaitGroup{}

			wg.Go(func() {
				if err := d.Start(ctx); err != nil {
					t.Fatalf("debouncer error: %s", err.Error())
				}
			})

			numOfUpdates := 100

			switch tc {
			case "endpointslices":
				epslice := &discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
					},
					Endpoints: make([]discoveryv1.Endpoint, 1),
				}

				addrEpslices := []string{}

				for i := range numOfUpdates {
					addrEpslices = append(addrEpslices, strconv.Itoa(i))
					epslice.Endpoints[0].Addresses = addrEpslices
					fw.Add(epslice)
				}
			case "endpoints":
				ep := &v1.Endpoints{ //nolint:staticcheck
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
					},
					Subsets: make([]v1.EndpointSubset, 1), //nolint:staticcheck
				}

				addrEp := []v1.EndpointAddress{}

				for i := range numOfUpdates {
					addrEp = append(addrEp, v1.EndpointAddress{IP: strconv.Itoa(i)})
					ep.Subsets[0].Addresses = addrEp
					fw.Add(ep)
				}
			case "services":
				svc := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
					},
				}

				svcPorts := []v1.ServicePort{}

				for i := range numOfUpdates {
					svcPorts = append(svcPorts, v1.ServicePort{Port: int32(i)})
					svc.Spec.Ports = svcPorts
					fw.Add(svc)
				}

			default:
				t.Fatal("unknown test", "type", tc)
			}

			out := <-d.output

			switch tc {
			case "endpointslices":
				outEps, ok := out.Object.(*discoveryv1.EndpointSlice)
				if !ok {
					t.Fatal("got different type of object than EndpointSlice, failed to cast")
				}

				if len(outEps.Endpoints[0].Addresses) != numOfUpdates {
					t.Fatalf("expected to aggregate %d events, but got %d", numOfUpdates, len(outEps.Endpoints[0].Addresses))
				}
			case "endpoints":
				outEps, ok := out.Object.(*v1.Endpoints) //nolint:staticcheck
				if !ok {
					t.Fatal("got different type of object than EndpointSlice, failed to cast")
				}

				if len(outEps.Subsets[0].Addresses) != numOfUpdates {
					t.Fatalf("expected to aggregate %d events, but got %d", numOfUpdates, len(outEps.Subsets[0].Addresses))
				}
			case "services":
				outSvc, ok := out.Object.(*v1.Service) //nolint:staticcheck
				if !ok {
					t.Fatal("got different type of object than EndpointSlice, failed to cast")
				}

				if len(outSvc.Spec.Ports) != numOfUpdates {
					t.Fatalf("expected to aggregate %d events, but got %d", numOfUpdates, len(outSvc.Spec.Ports))
				}
			default:
				t.Fatal("unknown test", "type", tc)
			}

			cancel()

			timedOut := waitTimeout(&wg, time.Second*3)

			if timedOut {
				t.Fatal("debouncer was not closed before timeout")
			}
		})
	}
}

func TestTypeChange(t *testing.T) {
	tcs := []string{"endpointslices", "endpoints", "services"}

	for _, tc := range tcs {
		t.Run(fmt.Sprintf("Get the newest event as the only one when using %s", tc), func(t *testing.T) {
			expected := "500ms"

			fw := watch.NewFake()
			defer fw.Stop()

			d, err := New(fw.ResultChan(), expected)

			if err != nil {
				t.Fatalf("failed to create debouncer with debounce time %q", expected)
			}

			if d.debounceTime.String() != expected {
				t.Fatalf("invalid debounce time %q was configured instead of expected %q", d.debounceTime.String(), expected)
			}

			ctx, cancel := context.WithCancel(context.Background())

			wg := sync.WaitGroup{}

			wg.Go(func() {
				if err := d.Start(ctx); err != nil {
					t.Fatalf("debouncer error: %s", err.Error())
				}
			})

			numOfUpdates := 100

			switch tc {
			case "endpointslices":
				epslice := &discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
					},
					Endpoints: make([]discoveryv1.Endpoint, 1),
				}

				addrEpslices := []string{}

				for i := range numOfUpdates {
					addrEpslices = append(addrEpslices, strconv.Itoa(i))
					epslice.Endpoints[0].Addresses = addrEpslices
					if i < numOfUpdates-1 {
						fw.Add(epslice)
					} else {
						fw.Delete(epslice)
					}
				}
			case "endpoints":
				ep := &v1.Endpoints{ //nolint:staticcheck
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
					},
					Subsets: make([]v1.EndpointSubset, 1), //nolint:staticcheck
				}

				addrEp := []v1.EndpointAddress{}

				for i := range numOfUpdates {
					addrEp = append(addrEp, v1.EndpointAddress{IP: strconv.Itoa(i)})
					ep.Subsets[0].Addresses = addrEp
					if i < numOfUpdates-1 {
						fw.Add(ep)
					} else {
						fw.Delete(ep)
					}
				}
			case "services":
				svc := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test",
						Namespace: "test",
					},
				}

				svcPorts := []v1.ServicePort{}

				for i := range numOfUpdates {
					svcPorts = append(svcPorts, v1.ServicePort{Port: int32(i)})
					svc.Spec.Ports = svcPorts
					if i < numOfUpdates-1 {
						fw.Add(svc)
					} else {
						fw.Delete(svc)
					}
				}

			default:
				t.Fatal("unknown test", "type", tc)
			}

			out := <-d.output

			if out.Type != watch.Added {
				t.Fatalf("expected to get add event, but got %s event", out.Type)
			}

			out = <-d.output

			if out.Type != watch.Deleted {
				t.Fatalf("expected to get delete event, but got %s event", out.Type)
			}

			cancel()

			timedOut := waitTimeout(&wg, time.Second*3)

			if timedOut {
				t.Fatal("debouncer was not closed before timeout")
			}
		})
	}
}

// waitTimeout waits for the waitgroup for the specified max timeout.
// Returns true if waiting timed out.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(timeout):
		return true // timed out
	}
}
