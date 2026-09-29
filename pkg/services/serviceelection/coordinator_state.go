package serviceelection

import (
	"github.com/kube-vip/kube-vip/pkg/lease"
	"k8s.io/apimachinery/pkg/types"
)

// coordinatorMembership is the membership and shared-Lease state protected by
// coordinator.mutex.
type coordinatorMembership struct {
	members map[types.UID]*member

	// lease is the Lease used by the current campaign. While campaigns.current is
	// non-nil, it must remain unchanged until completeCampaign finishes that
	// campaign.
	lease *lease.Lease
}

// coordinatorCampaignState is the campaign and retry state protected by
// coordinator.mutex.
type coordinatorCampaignState struct {
	// current is created for membership.lease. That Lease generation remains
	// attached to the campaign until completeCampaign clears current.
	current *campaign

	// restartFailures counts consecutive campaigns that ended via
	// cancelCampaign (an activation failure with no other ready member)
	// rather than a normal leadership change. It backs off campaign restarts
	// and resets on the next successful activation.
	restartFailures int
}
