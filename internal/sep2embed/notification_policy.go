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
func buildNotifier(subs coresub.SubscriptionLister, workers, queueSize int, allowLoopback bool, timeouts coresub.NotificationTimeouts) *coresub.Manager {
	policy := coresub.DestinationPolicy{AllowLoopback: allowLoopback}
	if policy.AllowLoopback {
		log.Print("WARNING: SEP2_NOTIFICATION_ALLOW_LOOPBACK / -sep2-notification-allow-loopback is set: " +
			"subscriptions may target any loopback destination (127.0.0.0/8, ::1, or a hostname such as " +
			"localhost that resolves there) on any port, reaching every service this bridge's network " +
			"namespace exposes there, including its own STOMP broker, IEEE 2030.5 listener and admin UI " +
			"among others; the admin UI's Bearer auth does not narrow this; intended for test harnesses only")
	}
	return coresub.NewManager(subs, workers, queueSize, coresub.WithDestinationPolicy(policy), coresub.WithNotificationTimeouts(timeouts))
}
