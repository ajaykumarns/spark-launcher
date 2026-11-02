package menubar

import "time"

// ModelStatus is one model's actual state as reported by the controller.
// ActiveRequests is nil when the controller did not report a count; unknown
// is never treated as zero.
type ModelStatus struct {
	ID             string
	Name           string
	State          string
	Nodes          []string
	Loaded         bool
	ActiveRequests *int
}

// DeviceStatus is one Spark's host memory. Bytes are nil when the controller
// did not report a usable reading.
type DeviceStatus struct {
	ID             string
	TotalBytes     *uint64
	AvailableBytes *uint64
}

// Choice is one loadable catalog entry. ID is the canonical catalog model ID,
// which is also the exact argument passed to `sparky load`.
type Choice struct {
	ID   string
	Name string
}

// Snapshot is a deep, immutable view of the last known controller state.
// Error carries the most recent refresh failure while Models/Devices keep
// the last successful data. Loading is true from the moment a load is
// accepted until its result has been recorded. LoadError/LoadMessage carry
// the last load outcome.
type Snapshot struct {
	Models      []ModelStatus
	Devices     []DeviceStatus
	UpdatedAt   time.Time
	Error       string
	Loading     bool
	LoadError   string
	LoadMessage string
}
