package sep2embed

import (
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/store/memory"
)

// newStores builds a fully-populated assembly.Stores, matching how the
// server-of-record assembles its own Stores (and core's own
// assembly_test.go testStores helper): every field is a plain in-memory
// store with no persistence path. Populating every field (rather than
// leaving optional function-set stores nil) mirrors the server-of-record
// so the embedded router mounts the full spec-compliant route set, not a
// reduced one; assembly.BuildProtocolRouter treats a nil field as "skip
// this function set's routes", which we do not want here.
func newStores() *assembly.Stores {
	return &assembly.Stores{
		EndDevices:          memory.NewEndDeviceStore(),
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
