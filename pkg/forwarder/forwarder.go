package forwarder

import (
	"context"

	log "github.com/sirupsen/logrus"
)

type Forwarder interface {
	Start(context.Context) error
}

type forwarder struct {
	srcAddress           string
	srcPort              int
	dstAddress           string
	dstPort              int
	ipvs                 bool
	ipvsForwardingMethod string
}

func NewForwarder(srcAddress string, srcPort int, dstAddress string, dstPort int, ipvs bool, ipvsForwardingMethod string) Forwarder {
	return &forwarder{
		srcAddress:           srcAddress,
		srcPort:              srcPort,
		dstAddress:           dstAddress,
		dstPort:              dstPort,
		ipvs:                 ipvs,
		ipvsForwardingMethod: ipvsForwardingMethod,
	}
}

func (f *forwarder) Start(ctx context.Context) error {
	log.Infoln("starting forwarder")
	// If backend address is quad-zero route, early return
	// (destination address is unknown, can't setup forwarding).
	// If at some point we want to prepare forwarding none the less we would have to pick up any interface
	// address and continue with it. If the user want to profit from different vip port and backend port
	// it's probably easier the user define the backend address instead
	if f.dstAddress == "0.0.0.0" {
		return nil
	}
	go func(ctx context.Context, stop func() error) {
		<-ctx.Done()
		if err := stop(); err != nil {
			log.Errorf("forwarder stop error: %v", err)
		}
	}(ctx, f.Stop)
	if f.ipvs {
		return startIPVS(f.srcAddress, f.srcPort, f.dstAddress, f.dstPort, f.ipvsForwardingMethod)
	}
	startIPTables(ctx, f.srcAddress, f.srcPort, f.dstAddress, f.dstPort)
	return nil
}

func (f *forwarder) Stop() error {
	log.Infof("stopping forwarder")
	if f.dstAddress == "0.0.0.0" {
		return nil
	}
	if f.ipvs {
		return stopIPVS(f.srcAddress, f.srcPort, f.dstAddress, f.dstPort, f.ipvsForwardingMethod)
	}
	return stopIPTables(f.srcAddress, f.srcPort, f.dstAddress, f.dstPort)
}
