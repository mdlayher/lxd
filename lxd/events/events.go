package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pborman/uuid"

	"github.com/gorilla/websocket"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
)

// EventSource indicates the source of an event.
type EventSource uint8

// EventSourceLocal indicates the event was generated locally.
const EventSourceLocal = 0

// EventSourcePull indicates the event was received from an outbound event listener stream.
const EventSourcePull = 1

// EventSourcePush indicates the event was received from an event listener client connected to us.
const EventSourcePush = 2

// Server represents an instance of an event server.
type Server struct {
	serverCommon

	listeners map[string]*Listener
}

// NewServer returns a new event server.
func NewServer(debug bool, verbose bool) *Server {
	server := &Server{
		serverCommon: serverCommon{
			debug:   debug,
			verbose: verbose,
		},
		listeners: map[string]*Listener{},
	}

	return server
}

// AddListener creates and returns a new event listener.
func (s *Server) AddListener(projectName string, allProjects bool, connection *websocket.Conn, messageTypes []string, location string, excludeSources []EventSource, recvFunc EventHandler) (*Listener, error) {
	if allProjects && projectName != "" {
		return nil, fmt.Errorf("Cannot specify project name when listening for events on all projects")
	}

	ctx, ctxCancel := context.WithCancel(context.Background())

	listener := &Listener{
		listenerCommon: listenerCommon{
			Conn:         connection,
			messageTypes: messageTypes,
			ctx:          ctx,
			ctxCancel:    ctxCancel,
			id:           uuid.New(),
			recvFunc:     recvFunc,
		},

		location:       location,
		allProjects:    allProjects,
		projectName:    projectName,
		excludeSources: excludeSources,
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	if s.listeners[listener.id] != nil {
		return nil, fmt.Errorf("A listener with ID %q already exists", listener.id)
	}

	s.listeners[listener.id] = listener

	go listener.heartbeat()

	return listener, nil
}

// SendLifecycle broadcasts a lifecycle event.
func (s *Server) SendLifecycle(projectName string, event api.EventLifecycle) {
	s.Send(projectName, "lifecycle", event)
}

// Send broadcasts a custom event.
func (s *Server) Send(projectName string, eventType string, eventMessage interface{}) error {
	encodedMessage, err := json.Marshal(eventMessage)
	if err != nil {
		return err
	}
	event := api.Event{
		Type:      eventType,
		Timestamp: time.Now(),
		Metadata:  encodedMessage,
		Project:   projectName,
	}

	return s.broadcast(event, EventSourceLocal)
}

// Forward to the local events dispatcher an event received from another node.
func (s *Server) Forward(id int64, event api.Event) {
	if event.Type == "logging" {
		// Parse the message
		logEntry := api.EventLogging{}
		err := json.Unmarshal(event.Metadata, &logEntry)
		if err != nil {
			return
		}

		if !s.debug && logEntry.Level == "dbug" {
			return
		}

		if !s.debug && !s.verbose && logEntry.Level == "info" {
			return
		}
	}

	err := s.broadcast(event, EventSourcePull)
	if err != nil {
		logger.Warnf("Failed to forward event from member %d: %v", id, err)
	}
}

func (s *Server) broadcast(event api.Event, eventSource EventSource) error {
	sourceInSlice := func(source EventSource, sources []EventSource) bool {
		for _, i := range sources {
			if source == i {
				return true
			}
		}

		return false
	}

	s.lock.Lock()
	listeners := s.listeners
	for _, listener := range listeners {
		// If the event is project specific, check if the listener is requesting events from that project.
		if event.Project != "" && !listener.allProjects && event.Project != listener.projectName {
			continue
		}

		if sourceInSlice(eventSource, listener.excludeSources) {
			continue
		}

		if !shared.StringInSlice(event.Type, listener.messageTypes) {
			continue
		}

		go func(listener *Listener, event api.Event) {
			// Check that the listener still exists
			if listener == nil {
				return
			}

			// Make sure we're not done already
			if listener.IsClosed() {
				return
			}

			// Set the Location to the expected serverName
			if event.Location == "" {
				eventCopy := api.Event{}
				err := shared.DeepCopy(&event, &eventCopy)
				if err != nil {
					return
				}
				eventCopy.Location = listener.location

				event = eventCopy
			}

			listener.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := listener.WriteJSON(event)
			if err != nil {
				// Remove the listener from the list
				s.lock.Lock()
				delete(s.listeners, listener.id)
				s.lock.Unlock()

				listener.Close()
			}
		}(listener, event)
	}
	s.lock.Unlock()

	return nil
}

// Listener describes an event listener.
type Listener struct {
	listenerCommon

	location       string
	allProjects    bool
	projectName    string
	excludeSources []EventSource
}
