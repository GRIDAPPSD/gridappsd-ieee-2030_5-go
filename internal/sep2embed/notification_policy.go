package sep2embed

import (
	"log"

	coresub "github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/handlers/subscription"
)

// buildNotifier constructs the subscription fan-out Manager under the
// destination policy allowLoopback selects. buildHandler passes the
// returned Manager to both creation-time notificationURI validation and
// delivery (assembly.ResourceNotifier), so this is the ONLY place the
// policy is built: creation and delivery can never diverge. Mirrors
// server-go's internal/server.newSubscriptionNotifier.
func buildNotifier(subs coresub.SubscriptionLister, workers, queueSize int, allowLoopback bool) *coresub.Manager {
	policy := coresub.DestinationPolicy{AllowLoopback: allowLoopback}
	if policy.AllowLoopback {
		log.Print("WARNING: SEP2_NOTIFICATION_ALLOW_LOOPBACK=true: subscriptions may target loopback addresses, " +
			"including this bridge's own admin UI listener (SEP2_ADMIN_UI_ADDR, loopback by default); " +
			"intended for test harnesses only")
	}
	return coresub.NewManager(subs, workers, queueSize, coresub.WithDestinationPolicy(policy))
}
