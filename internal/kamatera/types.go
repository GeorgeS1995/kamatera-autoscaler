package kamatera

import "encoding/json"

// CreateServerRequest is the JSON body for POST /service/server.
// Field names match the Kamatera REST API.
type CreateServerRequest struct {
	Name             string `json:"name"`
	Password         string `json:"password,omitempty"`
	PasswordValidate string `json:"passwordValidate,omitempty"`
	SSHKey           string `json:"ssh-key,omitempty"`
	Datacenter       string `json:"datacenter"`
	Image            string `json:"image"`
	CPU              string `json:"cpu"`  // e.g. "2B" — cores + type letter
	RAM              int    `json:"ram"`  // MB
	Disk             string `json:"disk"` // e.g. "size=40"
	DailyBackup      string `json:"dailybackup,omitempty"`
	Managed          string `json:"managed,omitempty"`
	Network          string `json:"network"` // space-separated "name=...,ip=..." entries
	Quantity         int    `json:"quantity,omitempty"`
	BillingCycle     string `json:"billingcycle"` // "hourly" | "monthly"
	MonthlyPackage   string `json:"monthlypackage,omitempty"`
	PowerOn          string `json:"poweronaftercreate"` // "yes" | "no"
	ScriptFile       string `json:"script-file,omitempty"`
}

// CommandStatus is the lifecycle status of an asynchronous Kamatera command.
//
// Values are taken from observed live responses. Promotion from a raw string
// to a named type buys (a) compiler-level typo protection at call sites and
// (b) consolidation of the terminal/failure predicates into methods, so
// callers don't repeat `if s == "complete" || s == "completed"` everywhere.
type CommandStatus string

// Values are taken from the Kamatera OpenAPI example and live e2e responses
// (observed: "progress", "complete"). The other constants are documented
// state names from the OpenAPI but not yet observed in our traffic.
const (
	StatusInitializing CommandStatus = "initializing"
	StatusProgress     CommandStatus = "progress"
	StatusStarted      CommandStatus = "started"
	StatusComplete     CommandStatus = "complete"
	StatusError        CommandStatus = "error"
	StatusCancelled    CommandStatus = "cancelled"
)

// IsTerminal reports whether the command has reached an end state — successful
// or otherwise — and will not transition further.
func (s CommandStatus) IsTerminal() bool {
	switch s {
	case StatusComplete, StatusError, StatusCancelled:
		return true
	}
	return false
}

// IsFailure reports whether a terminal status indicates failure rather than
// successful completion.
func (s CommandStatus) IsFailure() bool {
	switch s {
	case StatusError, StatusCancelled:
		return true
	}
	return false
}

// QueueStatus models the response of GET /service/queue?id=<id>.
//
// Field types match real Kamatera responses (verified against live API output
// during e2e testing on 2026-05-02). Notable deviations from naive guesses:
//
//   - `id` is a JSON number, not a string (Server.ID is the string UUID).
//   - `server` is the server's *name*, not an embedded object.
type QueueStatus struct {
	ID          json.Number   `json:"id"`
	Status      CommandStatus `json:"status"`
	Server      string        `json:"server,omitempty"`
	Description string        `json:"description,omitempty"` // e.g. "Create Server", "Terminate Server"
	Added       string        `json:"added,omitempty"`
	Executed    string        `json:"executed,omitempty"`
	Completed   string        `json:"completed,omitempty"`
	Log         string        `json:"log,omitempty"`
}

// Server is a minimal projection of a Kamatera server populated from the
// console API GET /servers endpoint.
//
// We deliberately omit IPs and disk info: the autoscaler never needs them.
// Cluster joining happens through the cloud-init script *inside* the VM
// (which connects outbound to the control plane), and our control loop only
// matches K8s nodes to expected names. If a future caller needs richer
// detail (per-NIC IPs, disk sizes), GET /server/{serverId} returns the full
// shape — wire it as a separate method then, with proper console-API field
// names (`networks: [{network, ips}]`, `disks: [...]`).
type Server struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Power      string `json:"power,omitempty"`
	Datacenter string `json:"datacenter,omitempty"`
}
