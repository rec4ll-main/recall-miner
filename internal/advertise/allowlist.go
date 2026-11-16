package advertise

// Allowlisted auto-derive sources.
// Auto mode must not fetch any URL outside this map.
// S1 (STUN) is listed but not implemented in v1.
const (
	SourceE1 = "E1"
	SourceE2 = "E2"
	SourceS1 = "S1" // pending STUN implementation
)

// HTTPSEchoAllowlist maps source IDs to exact HTTPS echo endpoints.
var HTTPSEchoAllowlist = map[string]string{
	SourceE1: "https://api.ipify.org",
	SourceE2: "https://ifconfig.me/ip",
}

// STUNAllowlist maps source IDs to STUN URIs (not implemented in v1).
var STUNAllowlist = map[string]string{
	SourceS1: "stun:stun.l.google.com:19302",
}
