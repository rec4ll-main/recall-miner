package registry

import (
	"sort"
	"strings"
	"sync"
	"time"

	"recall-miner/internal/config"
)

const serviceMaxAge = 60 * time.Second

type ServiceInfo struct {
	Name         string         `json:"name"`
	URL          string         `json:"url"`
	Capabilities []string       `json:"capabilities"`
	Models       []string       `json:"models"`
	Metadata     map[string]any `json:"metadata"`
	MachineID    string         `json:"machine_id"`
	Health       map[string]any `json:"health"`
	LastSeen     float64        `json:"last_seen"`
	lastSeenTime time.Time
}

type RegisterRequest struct {
	Name         string         `json:"name"`
	URL          string         `json:"url"`
	Capabilities []string       `json:"capabilities"`
	Models       []string       `json:"models,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	MachineID    string         `json:"machine_id,omitempty"`
}

type HeartbeatRequest struct {
	Name      string         `json:"name"`
	MachineID string         `json:"machine_id,omitempty"`
	Health    map[string]any `json:"health,omitempty"`
}

type Registry struct {
	mu       sync.RWMutex
	services map[string]*ServiceInfo
	dbOwners      map[string]string
	staleDBOwners map[string]string
}

func New() *Registry {
	return &Registry{services: map[string]*ServiceInfo{}, dbOwners: map[string]string{}, staleDBOwners: map[string]string{}}
}

func (r *Registry) Register(req RegisterRequest) ServiceInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	metadata := req.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	machineID := req.MachineID
	if machineID == "" {
		if value, ok := metadata["machine_id"].(string); ok {
			machineID = value
		}
	}
	info := &ServiceInfo{
		Name:         req.Name,
		URL:          config.TrimRight(req.URL),
		Capabilities: req.Capabilities,
		Models:       req.Models,
		Metadata:     metadata,
		MachineID:    machineID,
		Health:       map[string]any{},
		LastSeen:     float64(now.UnixNano()) / float64(time.Second),
		lastSeenTime: now,
	}
	r.services[req.Name] = info
	return *info
}

func (r *Registry) Touch(name, machineID string, health map[string]any) (*ServiceInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.services[name]
	if !ok || info.stale(time.Now()) {
		return nil, false
	}
	now := time.Now()
	info.LastSeen = float64(now.UnixNano()) / float64(time.Second)
	info.lastSeenTime = now
	if machineID != "" {
		info.MachineID = machineID
	}
	if health != nil {
		info.Health = health
	}
	return info, true
}

func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.services[name]; !ok {
		return false
	}
	delete(r.services, name)
	for dbID, owner := range r.dbOwners {
		if owner == name {
			delete(r.dbOwners, dbID)
		}
	}
	return true
}

func (r *Registry) List() map[string]ServiceInfo {
	r.purgeStale()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]ServiceInfo{}
	for name, info := range r.services {
		out[name] = *info
	}
	return out
}

func (r *Registry) ByCapability(capability string) []ServiceInfo {
	r.purgeStale()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []ServiceInfo{}
	for _, info := range r.services {
		if Contains(info.Capabilities, capability) {
			out = append(out, *info)
		}
	}
	return out
}

// ByCapabilityHealthy returns non-stale services advertising capability whose
// Healthy() check passes, sorted by name for stable selection. Use this for
// inference routing (embedding/rerank/generate). DB pin routing keeps using
// ByCapability so ownership and restore readiness stay explicit.
func (r *Registry) ByCapabilityHealthy(capability string) []ServiceInfo {
	now := time.Now()
	out := []ServiceInfo{}
	for _, info := range r.ByCapability(capability) {
		if info.Healthy(now) {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) PurgeStale() {
	r.purgeStale()
}

func (r *Registry) purgeStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for name, info := range r.services {
		if info.stale(now) {
			delete(r.services, name)
			// Move DB pins to staleDBOwners so ordinary routing stays blocked
			// until an explicit restore reassigns (miner#3).
			for dbID, owner := range r.dbOwners {
				if owner == name {
					r.staleDBOwners[dbID] = owner
					delete(r.dbOwners, dbID)
				}
			}
		}
	}
}

// StaleDBOwner reports a pin that outlived its sidecar (needs explicit restore).
func (r *Registry) StaleDBOwner(dbID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.staleDBOwners[dbID]
}

// ReassignDB after a successful restore clears the stale pin and sets the live owner.
func (r *Registry) ReassignDB(dbID, serviceName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.staleDBOwners, dbID)
	r.dbOwners[dbID] = serviceName
}


func (r *Registry) PinDB(dbID, serviceName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dbOwners[dbID] = serviceName
}

func (r *Registry) UnpinDB(dbID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dbOwners, dbID)
}

func (r *Registry) DBOwner(dbID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dbOwners[dbID]
}

func (s ServiceInfo) stale(now time.Time) bool {
	return now.Sub(s.lastSeenTime) > serviceMaxAge
}

// Healthy reports whether s is currently usable: it has been seen within
// serviceMaxAge and, when a heartbeat has reported an explicit status, that
// status is "ok". A service that registered but has not yet heartbeated is
// treated as healthy for serviceMaxAge after registration.
func (s ServiceInfo) Healthy(now time.Time) bool {
	if s.stale(now) {
		return false
	}
	if raw, ok := s.Health["status"]; ok {
		status, ok := raw.(string)
		return ok && strings.EqualFold(status, "ok")
	}
	return true
}

func Contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func IsTEIStyle(name string) bool {
	switch strings.ToLower(name) {
	case "tei", "cpu", "hf":
		return true
	default:
		return false
	}
}

func IsDBHTTPService(s ServiceInfo) bool {
	api, _ := s.Metadata["api"].(string)
	return api == "recall-db-v1"
}
