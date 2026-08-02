package sep2embed

// testRegistrationPIN is the fleet-wide registration PIN the tests in this
// package provision. It is an obvious dummy, not a plausible operator
// value: 000000 sums to a multiple of ten, so it also satisfies the
// digit-sum check-digit shape without looking like a real secret.
const testRegistrationPIN uint32 = 0

// testResolvePIN is the Config.ResolveRegistrationPIN a test supplies so
// seeding can provision a device. Seeding fails closed when no PIN
// resolves (sep.xsd:184 makes pIN minOccurs=1 in the Registration
// sequence), so every test that seeds a device must supply one; this
// helper keeps that from being restated at a dozen call sites.
func testResolvePIN(string) (uint32, bool) { return testRegistrationPIN, true }
