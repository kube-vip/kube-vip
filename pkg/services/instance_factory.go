package services

import (
	"context"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/instance"
	v1 "k8s.io/api/core/v1"
)

type serviceInstanceFactory interface {
	Create(context.Context, *v1.Service, *sync.WaitGroup) (*instance.Instance, error)
}
