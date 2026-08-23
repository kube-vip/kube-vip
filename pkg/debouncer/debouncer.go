package debouncer

import (
	"context"
	"fmt"
	log "log/slog"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	DefaultTime = "0s"
	minimalTime = time.Millisecond * 200
)

type debouncer struct {
	input    <-chan watch.Event
	output   chan watch.Event
	stopChan chan any
	stopOnce sync.Once
	// events holds event per namespace
	namespaces   sync.Map
	debounceTime time.Duration
}

type ns struct {
	sync.Map
	cnt atomic.Int64
}

func (n *ns) get(name string) (*object, bool) {
	value, exists := n.Load(name)
	if !exists {
		return nil, false
	}
	i, ok := value.(*object)
	if !ok {
		return nil, false
	}
	return i, true
}

func (n *ns) add(name string, output chan<- watch.Event) *object {
	i := newObject(output)
	n.Store(name, i)
	n.cnt.Add(1)
	return i
}

func (n *ns) del(name string, object *object) {
	if n.CompareAndDelete(name, object) {
		n.cnt.Add(-1)
	}
}

func New(input <-chan watch.Event, debounceTime string) (*debouncer, error) {
	dt, err := time.ParseDuration(debounceTime)
	if err != nil {
		// debouncer was configured with invalid unparsable value, return error
		return nil, fmt.Errorf("failed to parse debounce time configuration: %w", err)
	}
	if dt < minimalTime {
		if dt > 0 {
			log.Warn("configured debounce time is less than the minimal threshold of 200ms, debouncer will remain disabled", "config value", dt.String())
		}
		return nil, nil
	}
	return &debouncer{
		input:        input,
		output:       make(chan watch.Event),
		stopChan:     make(chan any),
		debounceTime: dt,
	}, nil
}

func (d *debouncer) Start(ctx context.Context) error {
	wg := sync.WaitGroup{}
	debouncerCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		wg.Wait()
		close(d.output)
	}()

	for {
		select {
		case <-debouncerCtx.Done():
			// return if debouncer context was cancelled
			return nil
		case <-d.stopChan:
			// return if Stop() was called
			return nil
		case tmp := <-d.input:
			// event has no type, probably error
			if tmp.Type == "" {
				return fmt.Errorf("get undefined object (input channel probably closed)")
			}

			var namespace, name string

			// type switch event object
			switch v := tmp.Object.(type) {
			case *discoveryv1.EndpointSlice:
				namespace = v.Namespace
				name = v.Name
			case *v1.Endpoints: //nolint:staticcheck
				namespace = v.Namespace
				name = v.Name
			case *v1.Service:
				namespace = v.Namespace
				name = v.Name
			default:
				return fmt.Errorf("objects of type %T are not supported", v)
			}

		processEvent:
			for {
				eventNs, exists := d.getNs(namespace)
				if !exists {
					// if not, create new map for the namespace
					eventNs = d.addNs(namespace)
				}

				// check if the object was previously reconciled
				eventObject, exists := eventNs.get(name)

				// if not and the event is not of type 'Deleted', create new object
				if !exists && tmp.Type != watch.Deleted {
					eventObject = eventNs.add(name, d.output)

					workerObject := eventObject
					workerNs := eventNs
					workerName := name
					workerNamespace := namespace
					workerObject.onStop = func() {
						// Remove the object before its worker can become receiver-less.
						workerNs.del(workerName, workerObject)
					}

					wg.Go(func() {
						// start deboucing events for this object
						workerObject.start(debouncerCtx, d.debounceTime)
						// if debouncer for the object ended - e.g. object was deleted - clean the map of objects
						workerNs.del(workerName, workerObject)
						// if namespace is empty, delete the namespace map
						if workerNs.cnt.Load() == 0 {
							d.delNs(workerNamespace, workerNs)
						}
					})
				}

				if eventObject == nil {
					break processEvent
				}

				// pass the watch event to the debouncer object
				select {
				case eventObject.input <- tmp:
					break processEvent
				case <-eventObject.stopChan:
					// The object stopped after the map lookup. Retry the event
					// against the newly-created object instead of dropping it.
					continue processEvent
				case <-debouncerCtx.Done():
					return nil
				}
			}
		}
	}
}

func (d *debouncer) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopChan)
	})
}

func (d *debouncer) Output() chan watch.Event {
	return d.output
}

func (d *debouncer) getNs(namespace string) (*ns, bool) {
	value, exists := d.namespaces.Load(namespace)
	if !exists {
		return nil, false
	}
	n, ok := value.(*ns)
	if !ok {
		return nil, false
	}
	return n, true
}

func (d *debouncer) addNs(namespace string) *ns {
	n := ns{}
	d.namespaces.Store(namespace, &n)
	return &n
}

func (d *debouncer) delNs(namespace string, ns *ns) {
	d.namespaces.CompareAndDelete(namespace, ns)
}

type object struct {
	input    chan watch.Event
	output   chan<- watch.Event
	stopChan chan any
	stopOnce sync.Once
	onStop   func()
}

func newObject(output chan<- watch.Event) *object {
	return &object{
		input:    make(chan watch.Event),
		output:   output,
		stopChan: make(chan any),
	}
}

func (o *object) start(ctx context.Context, debounceTime time.Duration) {
	t := time.NewTicker(debounceTime)

	var last *watch.Event

	defer func() {
		if last != nil {
			o.output <- *last
			last = nil
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// if context is done, return
			return
		case <-o.stopChan:
			// return if Stop() was called
			return
		case tmp := <-o.input:
			// if last event is known, but an event of another type arrived,
			// send out the previous event
			if last != nil && last.Type != tmp.Type {
				o.output <- *last
			}
			// save current event as the last event
			last = &tmp
			// reset the ticker to wait for more events
			t.Reset(debounceTime)
		case <-t.C:
			if last != nil {
				// on tick, if we have an event, send it out
				o.output <- *last
				// if the event is of type 'Deleted', stop the debouncer for the object
				if last.Type == watch.Deleted {
					o.stop()
				}
				// reset last known event, so it won't be send out twice
				last = nil
			}
		}
	}
}

func (o *object) stop() {
	o.stopOnce.Do(func() {
		if o.onStop != nil {
			o.onStop()
		}
		close(o.stopChan)
	})
}
