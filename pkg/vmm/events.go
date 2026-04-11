package vmm

import (
	"context"
	"net"
	"sync"
	"time"
)

// EventType identifies a VM lifecycle event.
type EventType string

const (
	EventCreated       EventType = "created"
	EventKernelLoaded  EventType = "kernel_loaded"
	EventDevicesReady  EventType = "devices_ready"
	EventCPUConfigured EventType = "cpu_configured"
	EventStarting      EventType = "starting"
	EventRunning       EventType = "running"
	EventPaused        EventType = "paused"
	EventResumed       EventType = "resumed"
	EventShutdown      EventType = "shutdown"
	EventHalted        EventType = "halted"
	EventError         EventType = "error"
	EventStopped       EventType = "stopped"
	EventSnapshot      EventType = "snapshot"
	EventRestored      EventType = "restored"
)

// Event is a timestamped VM lifecycle event.
type Event struct {
	Time    time.Time `json:"time"`
	Type    EventType `json:"type"`
	Message string    `json:"message,omitempty"`
}

// EventSource is the minimal event surface shared by local and remote VM handles.
type EventSource interface {
	Events(since time.Time) []Event
	Subscribe() (chan Event, func())
}

const maxEvents = 1000

// EventLog tracks timestamped events for a VM.
// Thread-safe, supports SSE subscribers.
type EventLog struct {
	mu     sync.Mutex
	events []Event
	subs   []chan Event
}

// NewEventLog creates an empty event log.
func NewEventLog() *EventLog {
	return &EventLog{}
}

// Emit appends an event and notifies all subscribers.
func (el *EventLog) Emit(typ EventType, msg string) {
	ev := Event{
		Time:    time.Now(),
		Type:    typ,
		Message: msg,
	}
	el.mu.Lock()
	el.events = append(el.events, ev)
	if len(el.events) > maxEvents {
		el.events = el.events[len(el.events)-maxEvents:]
	}
	// Copy subs slice so we can notify outside the lock.
	subs := make([]chan Event, len(el.subs))
	copy(subs, el.subs)
	el.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Subscriber too slow, drop event.
		}
	}
}

// Events returns events occurring after since. If since is zero, returns all.
func (el *EventLog) Events(since time.Time) []Event {
	el.mu.Lock()
	defer el.mu.Unlock()
	if since.IsZero() {
		out := make([]Event, len(el.events))
		copy(out, el.events)
		return out
	}
	var out []Event
	for _, ev := range el.events {
		if ev.Time.After(since) {
			out = append(out, ev)
		}
	}
	return out
}

// Subscribe returns a channel that receives new events and an unsubscribe function.
func (el *EventLog) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 64)
	el.mu.Lock()
	el.subs = append(el.subs, ch)
	el.mu.Unlock()
	return ch, func() {
		el.mu.Lock()
		defer el.mu.Unlock()
		for i, s := range el.subs {
			if s == ch {
				el.subs = append(el.subs[:i], el.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

// DeviceInfo describes an attached device.
type DeviceInfo struct {
	Type string `json:"type"`
	IRQ  int    `json:"irq"`
}

// Handle abstracts a running VM whether local or worker-backed.
type Handle interface {
	Start() error
	Stop()
	TakeSnapshot(string) (*Snapshot, error)
	State() State
	ID() string
	Uptime() time.Duration
	Events() EventSource
	VMConfig() Config
	DeviceList() []DeviceInfo
	ConsoleOutput() []byte
	WaitStopped(ctx context.Context) error
	// FirstOutputAt returns the wall-clock instant at which the guest
	// first transmitted a byte on the serial console (UART). Zero time
	// if the guest has not yet printed anything or is not tracked.
	FirstOutputAt() time.Time
}

type WorkerMetadata struct {
	Kind       string    `json:"kind"`
	SocketPath string    `json:"socket_path,omitempty"`
	WorkerPID  int       `json:"worker_pid,omitempty"`
	JailRoot   string    `json:"jail_root,omitempty"`
	RunDir     string    `json:"run_dir,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

type WorkerBacked interface {
	Handle
	WorkerMetadata() WorkerMetadata
}

type VsockDialer interface {
	DialVsock(port uint32) (net.Conn, error)
}
