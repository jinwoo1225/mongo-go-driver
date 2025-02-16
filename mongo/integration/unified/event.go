// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package unified

import (
	"strings"

	"go.mongodb.org/mongo-driver/event"
)

type monitoringEventType string

const (
	commandStartedEvent             monitoringEventType = "CommandStartedEvent"
	commandSucceededEvent           monitoringEventType = "CommandSucceededEvent"
	commandFailedEvent              monitoringEventType = "CommandFailedEvent"
	poolCreatedEvent                monitoringEventType = "PoolCreatedEvent"
	poolReadyEvent                  monitoringEventType = "PoolReadyEvent"
	poolClearedEvent                monitoringEventType = "PoolClearedEvent"
	poolClosedEvent                 monitoringEventType = "PoolClosedEvent"
	connectionCreatedEvent          monitoringEventType = "ConnectionCreatedEvent"
	connectionReadyEvent            monitoringEventType = "ConnectionReadyEvent"
	connectionClosedEvent           monitoringEventType = "ConnectionClosedEvent"
	connectionCheckOutStartedEvent  monitoringEventType = "ConnectionCheckOutStartedEvent"
	connectionCheckOutFailedEvent   monitoringEventType = "ConnectionCheckOutFailedEvent"
	connectionCheckedOutEvent       monitoringEventType = "ConnectionCheckedOutEvent"
	connectionCheckedInEvent        monitoringEventType = "ConnectionCheckedInEvent"
	serverDescriptionChangedEvent   monitoringEventType = "ServerDescriptionChangedEvent"
	serverHeartbeatFailedEvent      monitoringEventType = "ServerHeartbeatFailedEvent"
	serverHeartbeatStartedEvent     monitoringEventType = "ServerHeartbeatStartedEvent"
	serverHeartbeatSucceededEvent   monitoringEventType = "ServerHeartbeatSucceededEvent"
	topologyDescriptionChangedEvent monitoringEventType = "TopologyDescriptionChangedEvent"
)

func monitoringEventTypeFromString(eventStr string) (monitoringEventType, bool) {
	switch strings.ToLower(eventStr) {
	case "commandstartedevent":
		return commandStartedEvent, true
	case "commandsucceededevent":
		return commandSucceededEvent, true
	case "commandfailedevent":
		return commandFailedEvent, true
	case "poolcreatedevent":
		return poolCreatedEvent, true
	case "poolreadyevent":
		return poolReadyEvent, true
	case "poolclearedevent":
		return poolClearedEvent, true
	case "poolclosedevent":
		return poolClosedEvent, true
	case "connectioncreatedevent":
		return connectionCreatedEvent, true
	case "connectionreadyevent":
		return connectionReadyEvent, true
	case "connectionclosedevent":
		return connectionClosedEvent, true
	case "connectioncheckoutstartedevent":
		return connectionCheckOutStartedEvent, true
	case "connectioncheckoutfailedevent":
		return connectionCheckOutFailedEvent, true
	case "connectioncheckedoutevent":
		return connectionCheckedOutEvent, true
	case "connectioncheckedinevent":
		return connectionCheckedInEvent, true
	case "serverdescriptionchangedevent":
		return serverDescriptionChangedEvent, true
	case "serverheartbeatfailedevent":
		return serverHeartbeatFailedEvent, true
	case "serverheartbeatstartedevent":
		return serverHeartbeatStartedEvent, true
	case "serverheartbeatsucceededevent":
		return serverHeartbeatSucceededEvent, true
	case "topologydescriptionchangedevent":
		return topologyDescriptionChangedEvent, true
	default:
		return "", false
	}
}

func monitoringEventTypeFromPoolEvent(evt *event.PoolEvent) monitoringEventType {
	switch evt.Type {
	case event.PoolCreated:
		return poolCreatedEvent
	case event.PoolReady:
		return poolReadyEvent
	case event.PoolCleared:
		return poolClearedEvent
	case event.PoolClosedEvent:
		return poolClosedEvent
	case event.ConnectionCreated:
		return connectionCreatedEvent
	case event.ConnectionReady:
		return connectionReadyEvent
	case event.ConnectionClosed:
		return connectionClosedEvent
	case event.GetStarted:
		return connectionCheckOutStartedEvent
	case event.GetFailed:
		return connectionCheckOutFailedEvent
	case event.GetSucceeded:
		return connectionCheckedOutEvent
	case event.ConnectionReturned:
		return connectionCheckedInEvent
	default:
		return ""
	}
}
