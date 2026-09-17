package sep2embed

import (
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store/memory"
)

// newStores builds a fully-populated assembly.Stores, matching how the
// server-of-record assembles its own Stores (and core's own
// assembly_test.go testStores helper): every resource store is a plain
// in-memory store with no persistence path, so BuildProtocolRouter mounts
// the full spec-compliant route set rather than skipping a function set
// whose store field is nil. Two fields are deliberately left at their
// zero value: EndDeviceManagers (no cross-device delegation) and
// RegistrationPolicy (no PIN/pollRate provisioning; see its own doc
// comment on assembly.Stores).
func newStores() *assembly.Stores {
	return &assembly.Stores{
		EndDevices: memory.NewEndDeviceStore(),

		// Allocates the opaque URL index that addresses each EndDevice
		// ("/edev/3/rg") in place of its LFDI. Addressing only: device
		// identity remains the certificate-derived LFDI on the EndDevice
		// record, which is what acl.go's ownership gate compares against.
		//
		// No persistence path is configured, matching every other store here,
		// so assignments last one process lifetime and a restart re-addresses
		// the fleet. That is acceptable while client and server are assumed
		// to start fresh together. It stops being acceptable as soon as a
		// client outlives a bridge restart, at which point this should move
		// to memory.NewEndDeviceIndexWithPersistence, ideally folded into the
		// same per-device provisioning record that holds registration PINs
		// rather than a second file that can disagree with it.
		//
		// A stale URL is safe in either configuration: an index that resolves
		// to a device other than the caller's certificate identity is denied
		// by the ownership gate, never served.
		EndDeviceIndexes: memory.NewEndDeviceIndex(),

		Registrations:       memory.NewRegistrationStore(),
		MirrorUsagePoints:   memory.NewStore[sep2.MirrorUsagePoint](),
		MirrorMeterReadings: memory.NewScopedStore[sep2.MirrorMeterReading](),

		DERs:               memory.NewScopedStore[sep2.DER](),
		DERCapabilities:    memory.NewScopedStore[sep2.DERCapability](),
		DERSettings:        memory.NewScopedStore[sep2.DERSettings](),
		DERStatuses:        memory.NewScopedStore[sep2.DERStatus](),
		DERAvailabilities:  memory.NewScopedStore[sep2.DERAvailability](),
		DERPrograms:        memory.NewDERProgramStore(),
		DERControls:        memory.NewScopedStore[sep2.DERControl](),
		DefaultDERControls: memory.NewScopedStore[sep2.DefaultDERControl](),
		DERCurves:          memory.NewStore[sep2.DERCurve](),

		FSAs:          memory.NewScopedStore[sep2.FunctionSetAssignments](),
		AdminFSAs:     memory.NewAdminFSAStore(),
		Subscriptions: memory.NewSubscriptionStore(),

		UsagePoints:   memory.NewStore[sep2.UsagePoint](),
		MeterReadings: memory.NewScopedStore[sep2.MeterReading](),
		Readings:      memory.NewScopedStore[sep2.Reading](),
		ReadingTypes:  memory.NewStore[sep2.ReadingType](),

		Configurations:           memory.NewScopedStore[sep2.Configuration](),
		DeviceStatuses:           memory.NewScopedStore[sep2.DeviceStatus](),
		LogEvents:                memory.NewScopedStore[sep2.LogEvent](),
		PowerStatuses:            memory.NewScopedStore[sep2.PowerStatus](),
		MessagingPrograms:        memory.NewStore[sep2.MessagingProgram](),
		TextMessages:             memory.NewScopedStore[sep2.TextMessage](),
		FlowReservationRequests:  memory.NewScopedStore[sep2.FlowReservationRequest](),
		FlowReservationResponses: memory.NewScopedStore[sep2.FlowReservationResponse](),
		ResponseSets:             memory.NewStore[sep2.ResponseSet](),
		Responses:                memory.NewScopedStore[sep2.Response](),
	}
}
