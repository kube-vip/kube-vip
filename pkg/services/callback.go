package services

import (
	"errors"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

var errServiceCallbackRequired = errors.New("service callback is required")

// Callback handles one Service for the lifetime of its service context.
type Callback func(*servicecontext.Context, *v1.Service, *sync.WaitGroup) error
