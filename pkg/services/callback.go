package services

import (
	"sync"

	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

type Callback struct {
	Function           func(*servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error
	UsesLeaderElection bool
}

func NewCallback(f func(*servicecontext.Context, *v1.Service, *sync.WaitGroup, bool) error, leaderElection bool) *Callback {
	return &Callback{
		Function:           f,
		UsesLeaderElection: leaderElection,
	}
}

func (c *Callback) Run(svcCtx *servicecontext.Context, svc *v1.Service, wg *sync.WaitGroup) error {
	return c.Function(svcCtx, svc, wg, c.UsesLeaderElection)
}
